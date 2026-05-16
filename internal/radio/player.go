package radio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kidixdev/lofi-radio/internal/config"
)

var errPlayerNotRunning = errors.New("player is not running")

// NumBands is the number of frequency bands exposed to the visualizer.
const NumBands = 32

// PCM capture parameters.
const (
	pcmSampleRate = 22050
	pcmChannels   = 1
	pcmChunkBytes = 512
)

// VisualizerBands holds the current smoothed per-band amplitudes in [0, 1].
type VisualizerBands struct {
	bandsV atomic.Value
	active int32 // atomic: 1 while PCM goroutine is running
	// lastUpdateUnixNano stores when the latest band frame was published.
	lastUpdateUnixNano int64
}

func (v *VisualizerBands) set(b [NumBands]float64) {
	v.bandsV.Store(b)
	atomic.StoreInt64(&v.lastUpdateUnixNano, time.Now().UnixNano())
}

// Get returns a snapshot of the current band amplitudes.
func (v *VisualizerBands) Get() [NumBands]float64 {
	raw := v.bandsV.Load()
	if raw == nil {
		return [NumBands]float64{}
	}
	return raw.([NumBands]float64)
}

// IsActive returns true while the PCM sampler goroutine is alive.
func (v *VisualizerBands) IsActive() bool {
	return atomic.LoadInt32(&v.active) == 1
}

// IsFresh reports whether a recent analysis frame has been published.
func (v *VisualizerBands) IsFresh(maxAge time.Duration) bool {
	ts := atomic.LoadInt64(&v.lastUpdateUnixNano)
	if ts == 0 {
		return false
	}
	last := time.Unix(0, ts)
	return time.Since(last) <= maxAge
}

// HasData reports whether at least one analyzer frame has been published.
func (v *VisualizerBands) HasData() bool {
	return atomic.LoadInt64(&v.lastUpdateUnixNano) != 0
}

// ---------------------------------------------------------------------------
// Player
// ---------------------------------------------------------------------------

type Player struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	waitCh    chan error
	volume    int
	streamURL string

	pcmCancel chan struct{}

	// Viz is the exported visualizer state the TUI reads every frame.
	Viz VisualizerBands
}

func NewPlayer(initialVolume int) *Player {
	p := &Player{volume: clampVolume(initialVolume)}
	p.Viz.set([NumBands]float64{})
	return p
}

func (p *Player) Play(streamURL string) error {
	p.mu.Lock()
	volume := p.volume
	p.mu.Unlock()
	return p.playWithVolume(streamURL, volume)
}

func (p *Player) playWithVolume(streamURL string, volume int) error {
	if err := p.Stop(); err != nil {
		return err
	}

	if u, err := url.Parse(streamURL); err == nil {
		writeLog("player.play.start host=%q path=%q volume=%d", u.Host, u.Path, volume)
	} else {
		writeLog("player.play.start volume=%d", volume)
	}

	// ── Main audio via ffplay ────────────────────────────────────────────────
	cmd := exec.Command(
		config.FFplayPath(),
		"-nodisp",
		"-autoexit",
		"-loglevel", "error",
		"-volume", strconv.Itoa(clampVolume(volume)),
		streamURL,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		writeLog("player.play.ffplay_start_error err=%v", err)
		return err
	}
	writeLog("player.play.ffplay_started pid=%d", cmd.Process.Pid)

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
		close(waitCh)
	}()

	p.mu.Lock()
	p.cmd = cmd
	p.stdin = stdinPipe
	p.waitCh = waitCh
	p.volume = clampVolume(volume)
	p.streamURL = streamURL
	p.mu.Unlock()

	// ── Parallel PCM capture for visualizer ─────────────────────────────────
	p.startPCMCapture(streamURL)
	return nil
}

// startPCMCapture launches a secondary ffmpeg that pipes raw PCM to the
// analysis goroutine.
func (p *Player) startPCMCapture(streamURL string) {
	cancel := make(chan struct{})
	p.mu.Lock()
	p.pcmCancel = cancel
	p.mu.Unlock()

	atomic.StoreInt32(&p.Viz.active, 1)

	go func() {
		defer atomic.StoreInt32(&p.Viz.active, 0)
		writeLog("viz.capture.start")
		type pcmProfile struct {
			name          string
			withReconnect bool
			strictMap     bool
		}
		profiles := []pcmProfile{
			{name: "plain+auto_map", withReconnect: false, strictMap: false},
			{name: "plain+strict", withReconnect: false, strictMap: true},
			{name: "reconnect+auto_map", withReconnect: true, strictMap: false},
			{name: "reconnect+strict", withReconnect: true, strictMap: true},
		}

		attempt := 0
		for {
			select {
			case <-cancel:
				return
			default:
			}

			profile := profiles[attempt%len(profiles)]
			attempt++

			ffmpeg, stdout, err := startPCMFFmpeg(streamURL, profile.withReconnect, profile.strictMap)
			if err != nil {
				writeLog("viz.capture.profile_failed profile=%q err=%v", profile.name, err)
				time.Sleep(800 * time.Millisecond)
				continue
			}
			if ffmpeg.Process != nil {
				writeLog("viz.capture.started pid=%d profile=%q", ffmpeg.Process.Pid, profile.name)
			}

			// Terminate ffmpeg when cancelled.
			doneKill := make(chan struct{})
			go func(cmd *exec.Cmd) {
				defer close(doneKill)
				<-cancel
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			}(ffmpeg)

			p.runPCMAnalysis(stdout, cancel)
			waitErr := ffmpeg.Wait()
			<-doneKill
			if waitErr != nil {
				writeLog("viz.capture.ffmpeg_exit_err profile=%q err=%v", profile.name, waitErr)
			} else {
				writeLog("viz.capture.ffmpeg_exit_ok profile=%q", profile.name)
			}

			select {
			case <-cancel:
				return
			default:
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

func startPCMFFmpeg(streamURL string, withReconnect bool, strictMap bool) (*exec.Cmd, io.ReadCloser, error) {
	args := []string{
		"-loglevel", "error",
		"-nostdin",
		// Reduce demux/IO buffering so PCM reaches the analyzer continuously
		// instead of in large live-segment bursts.
		"-fflags", "+nobuffer",
		"-flags", "low_delay",
		"-flush_packets", "1",
		"-max_delay", "0",
		"-max_probe_packets", "1",
		"-rw_timeout", "15000000",
		"-probesize", "32k",
		"-analyzeduration", "0",
	}
	if withReconnect {
		args = append(args,
			"-reconnect", "1",
			"-reconnect_streamed", "1",
			"-reconnect_at_eof", "1",
			"-reconnect_delay_max", "2",
		)
	}
	args = append(args, "-i", streamURL, "-vn", "-sn", "-dn")
	if strictMap {
		args = append(args, "-map", "0:a:0")
	}
	args = append(args,
		"-acodec", "pcm_s16le",
		"-f", "s16le",
		"-ar", strconv.Itoa(pcmSampleRate),
		"-ac", strconv.Itoa(pcmChannels),
		"pipe:1",
	)

	ffmpeg := exec.Command(config.FFmpegPath(), args...)
	stdout, err := ffmpeg.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	var stderr bytes.Buffer
	ffmpeg.Stderr = &stderr
	if err := ffmpeg.Start(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, nil, err
	}
	return ffmpeg, stdout, nil
}

// runPCMAnalysis uses chunk-based amplitude/delta bins (reference logic style)
// instead of FFT, which is more tolerant for unstable live HLS chunks.
func (p *Player) runPCMAnalysis(r io.Reader, cancel chan struct{}) {
	const (
		bytesPerSample = 2
		scaleDivisor   = 11000.0
		bassBins       = 3
		noiseGate      = 0.014
		silenceFloor   = 220.0
		silenceFramesN = 8
	)
	rawBuf := make([]byte, pcmChunkBytes)
	frameSamples := pcmChunkBytes / bytesPerSample
	frameDur := time.Second * time.Duration(frameSamples) / time.Duration(pcmSampleRate)
	binState := make([]float64, NumBands)
	prevSamples := make([]int16, NumBands)
	binPeak := make([]float64, NumBands)
	silenceFrames := 0
	frames := 0
	lastLog := time.Now()
	var nextFrameAt time.Time

	for {
		select {
		case <-cancel:
			return
		default:
		}

		n, err := io.ReadFull(r, rawBuf)
		if err != nil {
			writeLog("viz.analysis.read_error err=%v", err)
			return
		}
		for i := range binPeak {
			binPeak[i] = 0
		}
		var out [NumBands]float64
		frameAbsSum := 0.0
		samplesFound := n / bytesPerSample
		for i := 0; i < samplesFound; i++ {
			base := i * 2
			sample := int16(binary.LittleEndian.Uint16(rawBuf[base:]))
			absSample := math.Abs(float64(sample))
			frameAbsSum += absSample
			for b := 0; b < NumBands; b++ {
				var value float64
				if b < bassBins {
					// Keep only a few true bass bins driven by amplitude.
					value = absSample
				} else {
					delta := math.Abs(float64(sample - prevSamples[b]))
					// Treble-ish bins react mostly to transients.
					value = delta * (0.8 + 1.8*float64(b)/float64(NumBands))
				}
				prevSamples[b] = sample

				if value > binPeak[b] {
					binPeak[b] = value
				}
			}
		}
		avgAbs := 0.0
		if samplesFound > 0 {
			avgAbs = frameAbsSum / float64(samplesFound)
		}
		if avgAbs < silenceFloor {
			silenceFrames++
		} else {
			silenceFrames = 0
		}

		for b := 0; b < NumBands; b++ {
			pos := float64(b) / float64(NumBands-1)
			decay := 0.84 - 0.10*pos
			if decay < 0.70 {
				decay = 0.70
			}
			attack := 0.80
			if silenceFrames >= silenceFramesN {
				// Harder decay in sustained silence/track transition to clear stale bars.
				binState[b] *= 0.45
			} else if binPeak[b] > binState[b] {
				binState[b] += attack * (binPeak[b] - binState[b])
			} else {
				binState[b] *= decay
			}

			v := binState[b] / scaleDivisor
			if v > 1 {
				v = 1
			}
			if v < noiseGate {
				v = 0
			}
			out[b] = v
		}

		p.Viz.set(out)
		frames++
		if time.Since(lastLog) >= 5*time.Second {
			writeLog("viz.analysis.heartbeat frames=%d", frames)
			frames = 0
			lastLog = time.Now()
		}

		// Pace analysis to audio-time so bursty live-segment delivery (e.g. ~5s HLS)
		// still renders as continuous motion in the TUI.
		now := time.Now()
		if nextFrameAt.IsZero() || now.Sub(nextFrameAt) > 250*time.Millisecond {
			nextFrameAt = now
		}
		nextFrameAt = nextFrameAt.Add(frameDur)
		sleepFor := time.Until(nextFrameAt)
		if sleepFor > 0 {
			timer := time.NewTimer(sleepFor)
			select {
			case <-cancel:
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}
}

// stopPCMCapture signals the analysis goroutine and clears the visualizer.
func (p *Player) stopPCMCapture() {
	p.mu.Lock()
	cancel := p.pcmCancel
	p.pcmCancel = nil
	p.mu.Unlock()

	if cancel != nil {
		close(cancel)
	}
	writeLog("viz.capture.stop")
	p.Viz.set([NumBands]float64{})
}

// RestartPCMCapture restarts only the visualizer PCM capture while keeping
// audio playback running.
func (p *Player) RestartPCMCapture() bool {
	p.mu.Lock()
	url := p.streamURL
	running := p.cmd != nil
	p.mu.Unlock()

	if !running || url == "" {
		writeLog("viz.capture.restart_skipped running=%t url_empty=%t", running, url == "")
		return false
	}
	writeLog("viz.capture.restart")
	p.stopPCMCapture()
	p.startPCMCapture(url)
	return true
}

// ---------------------------------------------------------------------------
// Playback control
// ---------------------------------------------------------------------------

func (p *Player) Stop() error {
	p.stopPCMCapture()

	p.mu.Lock()
	cmd := p.cmd
	stdinPipe := p.stdin
	waitCh := p.waitCh
	p.cmd = nil
	p.stdin = nil
	p.waitCh = nil
	p.streamURL = ""
	p.mu.Unlock()

	if cmd == nil {
		return nil
	}
	writeLog("player.stop")

	if stdinPipe != nil {
		_, _ = stdinPipe.Write([]byte{'q'})
		_ = stdinPipe.Close()
	}

	if waitCh != nil {
		select {
		case <-waitCh:
			writeLog("player.stop.ffplay_wait_ok")
			return nil
		case <-time.After(600 * time.Millisecond):
		}
	}

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if waitCh != nil {
		<-waitCh
	}
	writeLog("player.stop.ffplay_killed")
	return nil
}

func (p *Player) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}

func (p *Player) WaitChan() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitCh
}

func (p *Player) PauseToggle() error {
	return p.sendKey('p')
}

func (p *Player) IncreaseVolume(step int) (int, error) {
	if step <= 0 {
		step = 5
	}
	p.mu.Lock()
	p.volume = clampVolume(p.volume + step)
	vol := p.volume
	p.mu.Unlock()
	return vol, p.sendKey('0')
}

func (p *Player) DecreaseVolume(step int) (int, error) {
	if step <= 0 {
		step = 5
	}
	p.mu.Lock()
	p.volume = clampVolume(p.volume - step)
	vol := p.volume
	p.mu.Unlock()
	return vol, p.sendKey('9')
}

func (p *Player) Volume() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.volume
}

func (p *Player) sendKey(key byte) error {
	p.mu.Lock()
	stdinPipe := p.stdin
	p.mu.Unlock()
	if stdinPipe == nil {
		return errPlayerNotRunning
	}
	_, err := stdinPipe.Write([]byte{key})
	return err
}

func clampVolume(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
