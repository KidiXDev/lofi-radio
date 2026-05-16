package radio

import (
	"bytes"
	"context"
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

	"github.com/hajimehoshi/oto"
	"github.com/kidixdev/lofi-radio/internal/config"
)

var errPlayerNotRunning = errors.New("player is not running")
var preferredPCMProfile atomic.Value // string

type pcmProfile struct {
	name          string
	withReconnect bool
	strictMap     bool
}

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
	waitCh    chan error
	volume    int
	streamURL string
	stopCh    chan struct{}

	audioCtx     *oto.Context
	audioPlayer  PCMPlayer
	pauseFlag    int32
	ffmpegStderr *bytes.Buffer

	// Viz is the exported visualizer state the TUI reads every frame.
	Viz VisualizerBands
}

type PCMPlayer interface {
	io.Writer
	Close() error
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

	ctx, err := oto.NewContext(pcmSampleRate, pcmChannels, 2, pcmChunkBytes*4)
	if err != nil {
		return fmt.Errorf("create audio context: %w", err)
	}

	ffmpeg, stdout, ffmpegStderr, err := startPCMFFmpegWithFallback(streamURL)
	if err != nil {
		return err
	}
	if ffmpeg.Process != nil {
		writeLog("player.play.ffmpeg_started pid=%d", ffmpeg.Process.Pid)
	}

	audioPlayer := ctx.NewPlayer()
	waitCh := make(chan error, 1)
	stopCh := make(chan struct{})
	atomic.StoreInt32(&p.pauseFlag, 0)

	go func() {
		atomic.StoreInt32(&p.Viz.active, 1)
		defer atomic.StoreInt32(&p.Viz.active, 0)
		defer func() { _ = audioPlayer.Close() }()

		currentCmd := ffmpeg
		currentStream := stdout
		currentStderr := ffmpegStderr
		restarts := 0

		for {
			if isStopped(stopCh) {
				if currentCmd != nil && currentCmd.Process != nil {
					_ = currentCmd.Process.Kill()
				}
				if currentCmd != nil {
					_ = currentCmd.Wait()
				}
				waitCh <- nil
				break
			}

			err := p.runPCMPipeline(currentStream, audioPlayer)
			waitErr := currentCmd.Wait()
			if isStopped(stopCh) {
				waitCh <- nil
				break
			}

			// Any EOF / ffmpeg exit during live playback is treated as transient:
			// attempt fast restart on the same stream URL.
			restarts++
			writeLog("player.play.restart attempt=%d err=%v wait_err=%v", restarts, err, waitErr)
			time.Sleep(450 * time.Millisecond)

			nextCmd, nextStream, nextStderr, startErr := startPCMFFmpegWithFallback(streamURL)
			if startErr != nil {
				waitCh <- fmt.Errorf("restart stream failed after %d attempts: %w", restarts, startErr)
				break
			}

			p.mu.Lock()
			p.cmd = nextCmd
			p.ffmpegStderr = nextStderr
			p.mu.Unlock()

			currentCmd = nextCmd
			currentStream = nextStream
			currentStderr = nextStderr
			_ = currentStderr
		}
		close(waitCh)
	}()

	p.mu.Lock()
	p.cmd = ffmpeg
	p.waitCh = waitCh
	p.volume = clampVolume(volume)
	p.streamURL = streamURL
	p.stopCh = stopCh
	p.audioCtx = ctx
	p.audioPlayer = audioPlayer
	p.ffmpegStderr = ffmpegStderr
	p.mu.Unlock()

	return nil
}

func startPCMFFmpegWithFallback(streamURL string) (*exec.Cmd, io.Reader, *bytes.Buffer, error) {
	profiles := []pcmProfile{
		{name: "reconnect+strict", withReconnect: true, strictMap: true},
		{name: "reconnect+auto_map", withReconnect: true, strictMap: false},
		{name: "plain+strict", withReconnect: false, strictMap: true},
		{name: "plain+auto_map", withReconnect: false, strictMap: false},
	}
	// On YouTube HLS URLs, plain+auto_map is typically the fastest successful probe.
	profiles = prioritizeProfiles(profiles, "plain+auto_map")
	if raw := preferredPCMProfile.Load(); raw != nil {
		if lastGood, ok := raw.(string); ok && lastGood != "" {
			profiles = prioritizeProfiles(profiles, lastGood)
		}
	}

	var lastErr error
	for _, profile := range profiles {
		ffmpeg, stdout, stderrBuf, err := startPCMFFmpeg(streamURL, profile.withReconnect, profile.strictMap)
		if err != nil {
			lastErr = err
			writeLog("player.play.ffmpeg_profile_failed profile=%q err=%v", profile.name, err)
			continue
		}

		// Probe first PCM bytes so we don't keep a "running" process that never emits audio.
		probeBytes, probeErr := readFirstPCMChunk(stdout, ffmpeg, 8*time.Second)
		if probeErr != nil {
			lastErr = probeErr
			msg := ""
			if stderrBuf != nil {
				msg = strings.TrimSpace(stderrBuf.String())
			}
			if msg != "" {
				writeLog("player.play.ffmpeg_profile_probe_failed profile=%q err=%v stderr=%q", profile.name, probeErr, msg)
			} else {
				writeLog("player.play.ffmpeg_profile_probe_failed profile=%q err=%v", profile.name, probeErr)
			}
			continue
		}

		writeLog("player.play.ffmpeg_profile_ok profile=%q first_chunk=%d", profile.name, len(probeBytes))
		preferredPCMProfile.Store(profile.name)
		stream := io.MultiReader(bytes.NewReader(probeBytes), stdout)
		return ffmpeg, stream, stderrBuf, nil
	}
	if lastErr == nil {
		lastErr = errors.New("unable to start ffmpeg")
	}
	return nil, nil, nil, lastErr
}

func prioritizeProfiles(profiles []pcmProfile, preferred string) []pcmProfile {
	if preferred == "" || len(profiles) < 2 {
		return profiles
	}
	idx := -1
	for i, p := range profiles {
		if p.name == preferred {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return profiles
	}
	out := make([]pcmProfile, 0, len(profiles))
	out = append(out, profiles[idx])
	out = append(out, profiles[:idx]...)
	out = append(out, profiles[idx+1:]...)
	return out
}

func readFirstPCMChunk(stdout io.ReadCloser, ffmpeg *exec.Cmd, timeout time.Duration) ([]byte, error) {
	type probeResult struct {
		data []byte
		err  error
	}
	ch := make(chan probeResult, 1)
	go func() {
		buf := make([]byte, pcmChunkBytes)
		n, err := io.ReadAtLeast(stdout, buf, 2)
		if n > 0 {
			out := make([]byte, n)
			copy(out, buf[:n])
			ch <- probeResult{data: out, err: err}
			return
		}
		ch <- probeResult{err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case res := <-ch:
		if res.err != nil && !errors.Is(res.err, io.EOF) && !errors.Is(res.err, io.ErrUnexpectedEOF) {
			if ffmpeg.Process != nil {
				_ = ffmpeg.Process.Kill()
			}
			_ = ffmpeg.Wait()
			return nil, res.err
		}
		if len(res.data) < 2 {
			if ffmpeg.Process != nil {
				_ = ffmpeg.Process.Kill()
			}
			_ = ffmpeg.Wait()
			if res.err != nil {
				return nil, res.err
			}
			return nil, io.EOF
		}
		return res.data, nil
	case <-ctx.Done():
		if ffmpeg.Process != nil {
			_ = ffmpeg.Process.Kill()
		}
		_ = ffmpeg.Wait()
		return nil, fmt.Errorf("no PCM received within %s", timeout)
	}
}

func startPCMFFmpeg(streamURL string, withReconnect bool, strictMap bool) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
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
		"-reconnect_on_network_error", "1",
		"-reconnect_on_http_error", "4xx,5xx",
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
		return nil, nil, nil, err
	}
	stderr := &bytes.Buffer{}
	ffmpeg.Stderr = stderr
	if err := ffmpeg.Start(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, nil, nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, nil, nil, err
	}
	return ffmpeg, stdout, stderr, nil
}

// runPCMAnalysis uses chunk-based amplitude/delta bins (reference logic style)
// instead of FFT, which is more tolerant for unstable live HLS chunks.
func (p *Player) runPCMPipeline(r io.Reader, audioOut PCMPlayer) error {
	const (
		bytesPerSample = 2
		scaleDivisor   = 11000.0
		bassBins       = 3
		noiseGate      = 0.014
		silenceFloor   = 220.0
		silenceFramesN = 8
	)
	rawBuf := make([]byte, pcmChunkBytes)
	binState := make([]float64, NumBands)
	prevSamples := make([]int16, NumBands)
	binPeak := make([]float64, NumBands)
	silenceFrames := 0
	frames := 0
	lastLog := time.Now()

	for {
		n, err := io.ReadAtLeast(r, rawBuf, 2)
		if err != nil {
			if (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && n >= 2 {
				// Keep processing trailing bytes instead of dropping the last frame.
			} else {
				writeLog("pcm.pipeline.read_error err=%v", err)
				return err
			}
		}
		// Ensure sample-aligned buffer length for 16-bit PCM.
		if n%2 == 1 {
			n--
			if n == 0 {
				continue
			}
		}
		chunk := rawBuf[:n]
		volScale := float64(clampVolume(p.Volume())) / 100.0
		isPaused := atomic.LoadInt32(&p.pauseFlag) == 1
		playBuf := make([]byte, len(chunk))
		copy(playBuf, chunk)

		for i := range binPeak {
			binPeak[i] = 0
		}
		var bands [NumBands]float64
		frameAbsSum := 0.0
		samplesFound := n / bytesPerSample
		for i := 0; i < samplesFound; i++ {
			base := i * 2
			sample := int16(binary.LittleEndian.Uint16(chunk[base:]))
			audioSample := sample
			if isPaused {
				audioSample = 0
			} else {
				audioSample = int16(float64(sample) * volScale)
			}
			binary.LittleEndian.PutUint16(playBuf[base:], uint16(audioSample))
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
			bands[b] = v
		}

		p.Viz.set(bands)
		if _, err := audioOut.Write(playBuf); err != nil {
			return fmt.Errorf("write audio: %w", err)
		}
		frames++
		if time.Since(lastLog) >= 5*time.Second {
			writeLog("pcm.pipeline.heartbeat frames=%d", frames)
			frames = 0
			lastLog = time.Now()
		}
	}
}

// ---------------------------------------------------------------------------
// Playback control
// ---------------------------------------------------------------------------

func (p *Player) Stop() error {
	p.mu.Lock()
	cmd := p.cmd
	waitCh := p.waitCh
	stopCh := p.stopCh
	p.cmd = nil
	p.waitCh = nil
	p.streamURL = ""
	p.stopCh = nil
	atomic.StoreInt32(&p.pauseFlag, 0)
	player := p.audioPlayer
	p.audioPlayer = nil
	ctx := p.audioCtx
	p.audioCtx = nil
	p.ffmpegStderr = nil
	p.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}

	if cmd == nil {
		if player != nil {
			_ = player.Close()
		}
		if ctx != nil {
			_ = ctx.Close()
		}
		p.Viz.set([NumBands]float64{})
		return nil
	}
	writeLog("player.stop")

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if waitCh != nil {
		<-waitCh
	}
	if player != nil {
		_ = player.Close()
	}
	if ctx != nil {
		_ = ctx.Close()
	}
	p.Viz.set([NumBands]float64{})
	writeLog("player.stop.ffmpeg_killed")
	return nil
}

func (p *Player) IsRunning() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return false
	}
	if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
		return false
	}
	return true
}

func (p *Player) WaitChan() <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitCh
}

func isStopped(stopCh <-chan struct{}) bool {
	if stopCh == nil {
		return false
	}
	select {
	case <-stopCh:
		return true
	default:
		return false
	}
}

func (p *Player) PauseToggle() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return errPlayerNotRunning
	}
	if atomic.LoadInt32(&p.pauseFlag) == 1 {
		atomic.StoreInt32(&p.pauseFlag, 0)
	} else {
		atomic.StoreInt32(&p.pauseFlag, 1)
	}
	return nil
}

func (p *Player) IncreaseVolume(step int) (int, error) {
	if step <= 0 {
		step = 5
	}
	p.mu.Lock()
	p.volume = clampVolume(p.volume + step)
	vol := p.volume
	p.mu.Unlock()
	return vol, nil
}

func (p *Player) DecreaseVolume(step int) (int, error) {
	if step <= 0 {
		step = 5
	}
	p.mu.Lock()
	p.volume = clampVolume(p.volume - step)
	vol := p.volume
	p.mu.Unlock()
	return vol, nil
}

func (p *Player) Volume() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.volume
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
