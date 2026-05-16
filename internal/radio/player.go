package radio

import (
	"bytes"
	"context"
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
	fftSize       = 1024
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
		noiseGate      = 0.012
		minFreqHz      = 40.0
		maxFreqHz      = 9000.0
	)
	rawBuf := make([]byte, pcmChunkBytes)
	binState := make([]float64, NumBands) // smoothed output in [0,1]
	binNorm := make([]float64, NumBands)  // adaptive per-band reference
	fftIn := make([]float64, fftSize)
	fftPos := 0
	loudEnv := 0.0
	loudRef := 0.14
	lowBandEnv := 0.0
	frames := 0
	lastLog := time.Now()
	bandRanges := buildBandRanges(fftSize, pcmSampleRate, NumBands, minFreqHz, maxFreqHz)
	window := hannWindow(fftSize)
	for i := range binNorm {
		binNorm[i] = 0.22
	}

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

		var out [NumBands]float64
		samplesFound := n / bytesPerSample
		rmsAccum := 0.0
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
			// Keep analyzer fed with source PCM (not post-volume) for stable visuals.
			sampleNorm := float64(sample) / 32768.0
			rmsAccum += sampleNorm * sampleNorm
			fftIn[fftPos] = sampleNorm
			fftPos++
			if fftPos >= fftSize {
				fftPos = 0
				spec := fftMagnitudes(fftIn, window)
				bands := bandEnergies(spec, bandRanges)
				linMean := 0.0
				linVals := make([]float64, NumBands)
				for b := 0; b < NumBands; b++ {
					// dB-like compression: keeps crowd/noisy material from saturating all bars.
					db := 20.0 * math.Log10(1e-9+bands[b])
					if db < -90 {
						db = -90
					}
					lin := (db + 90) / 90 // 0..1
					linVals[b] = lin
					linMean += lin
				}
				linMean /= float64(NumBands)

				frameVals := make([]float64, NumBands)
				frameMax := 0.0
				frameMean := 0.0
				lowNow := 0.0
				for b := 0; b < NumBands; b++ {
					lin := linVals[b]
					// Per-band adaptive normalization with slower adaptation to avoid
					// flattening all bars to similar heights.
					if lin > binNorm[b] {
						binNorm[b] += 0.045 * (lin - binNorm[b]) // slow attack
					} else {
						binNorm[b] *= 0.9993 // very slow decay
					}
					// Gain shaping: cap per-band auto gain so dense mixes don't
					// force every band high all the time.
					autoGain := 1.0 / (0.24 + 1.9*binNorm[b])
					if autoGain > 1.45 {
						autoGain = 1.45
					}
					if autoGain < 0.55 {
						autoGain = 0.55
					}

					pos := float64(b) / float64(NumBands-1)
					// Emphasize low end, but keep a clear high-band presence.
					bassBoost := 1.0 + 0.52*math.Exp(-5.5*pos)
					highLift := 0.92 + 0.34*math.Pow(pos, 1.15)

					v := lin * autoGain * bassBoost * highLift
					// Keep lower amplitudes visible while preventing heavy saturation.
					v = math.Pow(v, 1.32)
					if v < 0 {
						v = 0
					}
					frameVals[b] = v
					frameMean += v
					if v > frameMax {
						frameMax = v
					}
					if b < 6 {
						lowNow += v
					}
				}
				frameMean /= float64(NumBands)
				lowNow /= 6.0
				if lowNow > lowBandEnv {
					lowBandEnv += 0.40 * (lowNow - lowBandEnv)
				} else {
					lowBandEnv += 0.12 * (lowNow - lowBandEnv)
				}
				kickDelta := lowNow - lowBandEnv
				if kickDelta < 0 {
					kickDelta = 0
				}
				if kickDelta > 0.45 {
					kickDelta = 0.45
				}
				// Frame-relative normalization preserves peaks but creates clearer
				// low-to-high contrast across bands in dense mixes.
				if frameMax > 1e-6 {
					targetPeak := 0.80 + 0.14*loudEnv
					if targetPeak > 0.94 {
						targetPeak = 0.94
					}
					scale := targetPeak / frameMax
					for b := 0; b < NumBands; b++ {
						v := frameVals[b]
						// Competition curve: push near-mean bins down so only strong
						// spectral components rise high.
						contrastFloor := frameMean * 0.68
						v = (v - contrastFloor) / (frameMax - contrastFloor + 1e-6)
						if v < 0 {
							v = 0
						}
						v = math.Pow(v, 0.96)
						v *= scale
						// Kick transient boost: low bands should punch on drum hits.
						if b < 8 {
							lowPos := 1.0 - float64(b)/8.0
							v += kickDelta * (0.75 * lowPos)
						}
						// Keep right side alive: mild high-band floor tied to loudness.
						if b >= NumBands/2 {
							highPos := float64(b-NumBands/2) / float64(NumBands/2)
							v += (0.015 + 0.03*loudEnv) * (0.45 + 0.55*highPos)
						}
						// Dynamic gate: suppress tiny bins so they don't all appear high.
						dynGate := noiseGate + 0.10*linMean + 0.03*loudEnv
						if dynGate > 0.12 {
							dynGate = 0.12
						}
						if v < dynGate {
							v = 0
						}
						if v > 1 {
							v = 1
						}

						// Band smoothing: fast attack, moderate release to avoid staircase look.
						if v > binState[b] {
							binState[b] += 0.40 * (v - binState[b])
						} else {
							binState[b] *= 0.84
						}
						out[b] = binState[b]
					}
				} else {
					for b := 0; b < NumBands; b++ {
						binState[b] *= 0.87
						out[b] = binState[b]
					}
				}
			} else {
				for b := 0; b < NumBands; b++ {
					out[b] = binState[b]
				}
			}
		}

		// Real-world behavior: tie bar height to short-term loudness envelope so
		// quiet transitions bring spectrum down, even when spectral shape remains.
		chunkRMS := 0.0
		if samplesFound > 0 {
			chunkRMS = math.Sqrt(rmsAccum / float64(samplesFound))
		}
		if chunkRMS > loudRef {
			loudRef += 0.02 * (chunkRMS - loudRef) // slower rise
		} else {
			loudRef += 0.002 * (chunkRMS - loudRef) // very slow fall
		}
		if loudRef < 0.03 {
			loudRef = 0.03
		}
		envTarget := chunkRMS / (loudRef * 1.22)
		if envTarget > 1 {
			envTarget = 1
		}
		if envTarget < 0 {
			envTarget = 0
		}
		// Faster release so transition dips are visible quickly.
		if envTarget > loudEnv {
			loudEnv += 0.20 * (envTarget - loudEnv)
		} else {
			loudEnv += 0.45 * (envTarget - loudEnv)
		}
		// Keep small floor so quiet passages still move, without keeping all bars high.
		loudGain := 0.05 + 0.95*loudEnv
		for b := 0; b < NumBands; b++ {
			out[b] *= loudGain
		}

		p.Viz.set(out)
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

func hannWindow(n int) []float64 {
	w := make([]float64, n)
	if n <= 1 {
		return w
	}
	for i := 0; i < n; i++ {
		w[i] = 0.5 - 0.5*math.Cos((2*math.Pi*float64(i))/float64(n-1))
	}
	return w
}

func fftMagnitudes(samples []float64, window []float64) []float64 {
	n := len(samples)
	complexIn := make([]complex128, n)
	for i := 0; i < n; i++ {
		complexIn[i] = complex(samples[i]*window[i], 0)
	}
	fft(complexIn)
	out := make([]float64, n/2)
	for i := 0; i < n/2; i++ {
		out[i] = cmplx.Abs(complexIn[i])
	}
	return out
}

func fft(a []complex128) {
	n := len(a)
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j &= ^bit
		}
		j |= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wlen := complex(math.Cos(ang), math.Sin(ang))
		for i := 0; i < n; i += length {
			w := complex(1.0, 0.0)
			half := length / 2
			for j := 0; j < half; j++ {
				u := a[i+j]
				v := a[i+j+half] * w
				a[i+j] = u + v
				a[i+j+half] = u - v
				w *= wlen
			}
		}
	}
}

type bandRange struct {
	start int
	end   int
}

func buildBandRanges(fftSize, sampleRate, bands int, minHz, maxHz float64) []bandRange {
	out := make([]bandRange, bands)
	nyquist := float64(sampleRate) / 2
	if maxHz > nyquist {
		maxHz = nyquist
	}
	if minHz < 1 {
		minHz = 1
	}
	for b := 0; b < bands; b++ {
		t0 := float64(b) / float64(bands)
		t1 := float64(b+1) / float64(bands)
		f0 := minHz * math.Pow(maxHz/minHz, t0)
		f1 := minHz * math.Pow(maxHz/minHz, t1)
		i0 := int((f0 / nyquist) * float64(fftSize/2))
		i1 := int((f1 / nyquist) * float64(fftSize/2))
		if i0 < 1 {
			i0 = 1
		}
		if i1 <= i0 {
			i1 = i0 + 1
		}
		maxBin := fftSize/2 - 1
		if i0 > maxBin {
			i0 = maxBin
		}
		if i1 > maxBin {
			i1 = maxBin
		}
		out[b] = bandRange{start: i0, end: i1}
	}
	return out
}

func bandEnergies(spec []float64, ranges []bandRange) []float64 {
	out := make([]float64, len(ranges))
	for i, r := range ranges {
		if r.end <= r.start || r.start >= len(spec) {
			continue
		}
		end := r.end
		if end > len(spec) {
			end = len(spec)
		}
		sum := 0.0
		for k := r.start; k < end; k++ {
			sum += spec[k] * spec[k]
		}
		width := float64(end - r.start)
		if width > 0 {
			out[i] = math.Sqrt(sum / width)
		}
	}
	return out
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
