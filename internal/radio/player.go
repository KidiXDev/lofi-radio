package radio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/cmplx"
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
	fftSize       = 512  // must be a power of 2
	hopSize       = 128  // advance by hopSize samples per analysis frame (75% overlap)
)

// bandBoundaries[b], bandBoundaries[b+1] are the inclusive FFT-bin range for band b.
// Logarithmically spaced between ~20 Hz and Nyquist.
var bandBoundaries [NumBands + 1]int

func init() {
	nyquist := pcmSampleRate / 2
	minFreq := 20.0
	maxFreq := float64(nyquist)
	logMin := math.Log10(minFreq)
	logMax := math.Log10(maxFreq)
	for i := 0; i <= NumBands; i++ {
		frac := float64(i) / float64(NumBands)
		freq := math.Pow(10, logMin+frac*(logMax-logMin))
		bin := int(freq * float64(fftSize) / float64(pcmSampleRate))
		if bin > fftSize/2 {
			bin = fftSize / 2
		}
		bandBoundaries[i] = bin
	}
}

// precomputed Hann window.
var hannWindow [fftSize]float64

func init() {
	for i := 0; i < fftSize; i++ {
		hannWindow[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(fftSize-1)))
	}
}

// VisualizerBands holds the current smoothed per-band amplitudes in [0, 1].
type VisualizerBands struct {
	mu     sync.RWMutex
	bands  [NumBands]float64
	active int32 // atomic: 1 while PCM goroutine is running
	// lastUpdateUnixNano stores when the latest band frame was published.
	lastUpdateUnixNano int64
}

func (v *VisualizerBands) set(b [NumBands]float64) {
	v.mu.Lock()
	v.bands = b
	v.mu.Unlock()
	atomic.StoreInt64(&v.lastUpdateUnixNano, time.Now().UnixNano())
}

// Get returns a snapshot of the current band amplitudes.
func (v *VisualizerBands) Get() [NumBands]float64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.bands
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
// Cooley-Tukey in-place FFT (radix-2 DIT, iterative).
// Works on a slice of complex128 whose length must be a power of 2.
// ---------------------------------------------------------------------------
func fftInPlace(x []complex128) {
	n := len(x)

	// Bit-reversal permutation.
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}

	// Butterfly stages.
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wBase := complex(math.Cos(angle), math.Sin(angle))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			half := length >> 1
			for k := 0; k < half; k++ {
				u := x[i+k]
				v := x[i+k+half] * w
				x[i+k] = u + v
				x[i+k+half] = u - v
				w *= wBase
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Player
// ---------------------------------------------------------------------------

type Player struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	waitCh chan error
	volume int
	streamURL string

	pcmCancel chan struct{}

	// Viz is the exported visualizer state the TUI reads every frame.
	Viz VisualizerBands
}

func NewPlayer(initialVolume int) *Player {
	return &Player{volume: clampVolume(initialVolume)}
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
		"-rw_timeout", "15000000",
		"-probesize", "256k",
		"-analyzeduration", "1M",
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

// runPCMAnalysis is the hot analysis loop.
//
// Reads PCM in hopSize-sample hops (75% overlap with fftSize window),
// applies a Hann-windowed Cooley-Tukey FFT, maps bins to log-spaced bands,
// and publishes raw (un-smoothed) normalised band values.
//
// Smoothing / decay is intentionally NOT done here — it must be done in the
// consumer (TUI onTick) at a wall-clock rate, so that network bursts don't
// cause exponential decay to collapse bars to zero between bursts.
//
// All buffers are pre-allocated; zero heap allocs in steady state.
func (p *Player) runPCMAnalysis(r io.Reader, cancel chan struct{}) {
	const bytesPerSample = 2
	const hopBytes = hopSize * bytesPerSample

	ring := make([]float64, fftSize)
	ringPos := 0
	rawBuf := make([]byte, hopBytes)
	fftBuf := make([]complex128, fftSize)
	var mag [fftSize / 2]float64
	halfN := fftSize / 2
	frames := 0
	lastLog := time.Now()

	for {
		select {
		case <-cancel:
			return
		default:
		}

		if _, err := io.ReadFull(r, rawBuf); err != nil {
			writeLog("viz.analysis.read_error err=%v", err)
			return
		}

		// Decode PCM into ring buffer.
		for i := 0; i < hopSize; i++ {
			s := int16(binary.LittleEndian.Uint16(rawBuf[i*2:]))
			ring[ringPos] = float64(s) / 32768.0
			ringPos = (ringPos + 1) % fftSize
		}

		// Build Hann-windowed FFT input from ring buffer.
		for i := 0; i < fftSize; i++ {
			idx := (ringPos + i) % fftSize
			fftBuf[i] = complex(ring[idx]*hannWindow[i], 0)
		}

		// In-place FFT.
		fftInPlace(fftBuf)

		// Magnitude spectrum.
		norm := 1.0 / float64(fftSize)
		for k := 0; k < halfN; k++ {
			mag[k] = cmplx.Abs(fftBuf[k]) * norm
		}

		// Map bins → log-spaced bands (RMS per band).
		const softCeil = 0.12
		var out [NumBands]float64
		for b := 0; b < NumBands; b++ {
			lo := bandBoundaries[b]
			hi := bandBoundaries[b+1]
			if hi <= lo {
				hi = lo + 1
			}
			if hi > halfN {
				hi = halfN
			}
			var sumSq float64
			for k := lo; k < hi; k++ {
				sumSq += mag[k] * mag[k]
			}
			count := hi - lo
			if count > 0 {
				v := math.Sqrt(sumSq/float64(count)) / softCeil
				if v > 1 {
					v = 1
				}
				out[b] = math.Pow(v, 0.60)
			}
		}

		// Publish raw values — NO smoothing/decay here.
		p.Viz.set(out)
		frames++
		if time.Since(lastLog) >= 5*time.Second {
			writeLog("viz.analysis.heartbeat frames=%d", frames)
			frames = 0
			lastLog = time.Now()
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
