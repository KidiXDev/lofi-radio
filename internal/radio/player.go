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
}

// NumBands is the number of frequency bands exposed to the visualizer.
const NumBands = 32

// PCM capture parameters.
const (
	pcmSampleRate    = 48000 // Hz
	pcmChannels      = 2     // stereo
	pcmBitDepthBytes = 2     // 16-bit
	pcmChunkBytes    = 4096
	fftSize          = 1024
)

// VisualizerBands holds the current smoothed per-band amplitudes in [0, 1].
type VisualizerBands struct {
	bandsV atomic.Value
	active atomic.Int32 // atomic: 1 while PCM goroutine is running
	// lastUpdateUnixNano stores when the latest band frame was published.
	lastUpdateUnixNano atomic.Int64
}

func (v *VisualizerBands) set(b [NumBands]float64) {
	v.bandsV.Store(b)
	v.lastUpdateUnixNano.Store(time.Now().UnixNano())
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
	return v.active.Load() == 1
}

// IsFresh reports whether a recent analysis frame has been published.
func (v *VisualizerBands) IsFresh(maxAge time.Duration) bool {
	ts := v.lastUpdateUnixNano.Load()
	if ts == 0 {
		return false
	}
	last := time.Unix(0, ts)
	return time.Since(last) <= maxAge
}

// HasData reports whether at least one analyzer frame has been published.
func (v *VisualizerBands) HasData() bool {
	return v.lastUpdateUnixNano.Load() != 0
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
	audioOnce    *sync.Once
	pauseFlag    int32
	ffmpegStderr *bytes.Buffer
	fadePermille atomic.Int32

	// Viz is the exported visualizer state the TUI reads every frame.
	Viz VisualizerBands

	statsMu         sync.Mutex
	bytesWritten    int64
	rateSampleBytes int64
	rateSampleAt    time.Time
	rateKbps        float64
	reconnects      atomic.Int64
	lastWriteUnix   int64
	lastReconnect   atomic.Int64
}

type PCMPlayer interface {
	io.Writer
	Close() error
}

type PlaybackStats struct {
	OutputKbps    float64
	Reconnects    int64
	LastWrite     time.Time
	LastReconnect time.Time
	AnalyzerFresh bool
	Running       bool
}

func NewPlayer(initialVolume int) *Player {
	p := &Player{volume: clampVolume(initialVolume)}
	p.Viz.set([NumBands]float64{})
	p.fadePermille.Store(1000)
	p.resetStats()
	return p
}

func (p *Player) Play(streamURL string, reconnectOnInterrupt bool) error {
	p.mu.Lock()
	volume := p.volume
	p.mu.Unlock()
	return p.playWithVolume(streamURL, volume, reconnectOnInterrupt)
}

func (p *Player) playWithVolume(streamURL string, volume int, reconnectOnInterrupt bool) error {
	if err := p.fadeOutAndStop(420 * time.Millisecond); err != nil {
		return err
	}
	p.resetStats()
	if pcmBitDepthBytes != 1 && pcmBitDepthBytes != 2 {
		err := fmt.Errorf("unsupported bit depth for oto v1: %d (expected 1 or 2)", pcmBitDepthBytes)
		writeLog("playback.config_error err=%v", err)
		return err
	}

	if u, err := url.Parse(streamURL); err == nil {
		writeLog("playback.connecting host=%q volume=%d", u.Host, volume)
	} else {
		writeLog("playback.connecting volume=%d", volume)
	}

	ctx, err := oto.NewContext(pcmSampleRate, pcmChannels, pcmBitDepthBytes, pcmChunkBytes*4)
	if err != nil {
		return fmt.Errorf("create audio context: %w", err)
	}

	connectStartedAt := time.Now()
	ffmpeg, stdout, ffmpegStderr, profileName, err := startPCMFFmpegWithFallback(streamURL, false)
	if err != nil {
		writeLog("playback.connect_failed err=%v", err)
		return err
	}
	writeLog("playback.connected profile=%q startup_ms=%d", profileName, time.Since(connectStartedAt).Milliseconds())
	if ffmpeg.Process != nil {
		writeLog("playback.process_started pid=%d", ffmpeg.Process.Pid)
	}

	audioPlayer := ctx.NewPlayer()
	audioOnce := &sync.Once{}
	waitCh := make(chan error, 1)
	stopCh := make(chan struct{})
	atomic.StoreInt32(&p.pauseFlag, 0)
	p.fadePermille.Store(0)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				writeLog("playback.panic recovered=%v", recovered)
				waitCh <- fmt.Errorf("playback panic: %v", recovered)
			}
		}()
		p.Viz.active.Store(1)
		defer p.Viz.active.Store(0)
		defer audioOnce.Do(func() { _ = audioPlayer.Close() })

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
			if !reconnectOnInterrupt {
				if isNaturalPlaybackEnd(err, waitErr) {
					writeLog("playback.completed")
					waitCh <- nil
				} else {
					waitCh <- fmt.Errorf("playback interrupted: %w", firstNonNilErr(err, waitErr))
				}
				break
			}

			// Any EOF / ffmpeg exit during live playback is treated as transient:
			// attempt fast restart on the same stream URL.
			restarts++
			p.reconnects.Store(int64(restarts))
			p.lastReconnect.Store(time.Now().UnixNano())
			writeLog("playback.interrupted attempt=%d err=%v", restarts, firstNonNilErr(err, waitErr))
			restartStartedAt := time.Now()
			time.Sleep(450 * time.Millisecond)

			nextCmd, nextStream, nextStderr, profileName, startErr := startPCMFFmpegWithFallback(streamURL, true)
			if startErr != nil {
				writeLog("playback.reconnect_failed attempt=%d err=%v", restarts, startErr)
				waitCh <- fmt.Errorf("restart stream failed after %d attempts: %w", restarts, startErr)
				break
			}
			writeLog("playback.resumed attempt=%d profile=%q downtime_ms=%d", restarts, profileName, time.Since(restartStartedAt).Milliseconds())

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
	p.audioOnce = audioOnce
	p.ffmpegStderr = ffmpegStderr
	p.mu.Unlock()

	go p.fadeIn(320 * time.Millisecond)

	return nil
}

func startPCMFFmpegWithFallback(streamURL string, preferReconnect bool) (*exec.Cmd, io.Reader, *bytes.Buffer, string, error) {
	profiles := []pcmProfile{
		{name: "reconnect", withReconnect: true},
		{name: "plain", withReconnect: false},
	}
	if preferReconnect {
		// Mid-stream recoveries prioritize resilience over startup speed.
		profiles = prioritizeProfiles(profiles, "reconnect")
	} else {
		// Initial connect prioritizes faster first-audio.
		profiles = prioritizeProfiles(profiles, "plain")
		if raw := preferredPCMProfile.Load(); raw != nil {
			if lastGood, ok := raw.(string); ok && lastGood != "" {
				profiles = prioritizeProfiles(profiles, lastGood)
			}
		}
	}

	var lastErr error
	for _, profile := range profiles {
		ffmpeg, stdout, stderrBuf, err := startPCMFFmpeg(streamURL, profile.withReconnect)
		if err != nil {
			lastErr = err
			continue
		}

		// Probe first PCM bytes so we don't keep a "running" process that never emits audio.
		probeBytes, probeErr := readFirstPCMChunk(stdout, ffmpeg, 8*time.Second)
		if probeErr != nil {
			lastErr = probeErr
			continue
		}

		preferredPCMProfile.Store(profile.name)
		stream := io.MultiReader(bytes.NewReader(probeBytes), stdout)
		return ffmpeg, stream, stderrBuf, profile.name, nil
	}
	if lastErr == nil {
		lastErr = errors.New("unable to start ffmpeg")
	}
	return nil, nil, nil, "", lastErr
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
		defer func() {
			if recovered := recover(); recovered != nil {
				ch <- probeResult{err: fmt.Errorf("probe panic: %v", recovered)}
			}
		}()
		buf := make([]byte, pcmChunkBytes)
		n, err := io.ReadAtLeast(stdout, buf, pcmBitDepthBytes)
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
		if len(res.data) < pcmBitDepthBytes {
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

func startPCMFFmpeg(streamURL string, withReconnect bool) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	args := []string{
		"-loglevel", "error",
		"-nostdin",
		"-thread_queue_size", "2048",
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

func (p *Player) runPCMPipeline(r io.Reader, audioOut PCMPlayer) error {
	const (
		bytesPerSample = pcmBitDepthBytes
		minFreqHz      = 32.0
		maxFreqHz      = 15000.0
		pcmQueueChunks = 18
	)

	type pcmPacket struct {
		data []byte
		err  error
	}

	packetCh := make(chan pcmPacket, pcmQueueChunks)
	done := make(chan struct{})
	defer close(done)

	go func() {
		defer close(packetCh)
		rawBuf := make([]byte, pcmChunkBytes)
		for {
			n, err := io.ReadAtLeast(r, rawBuf, bytesPerSample)
			if rem := n % bytesPerSample; rem != 0 {
				n -= rem
			}

			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, rawBuf[:n])
				select {
				case packetCh <- pcmPacket{data: chunk}:
				case <-done:
					return
				}
			}

			if err != nil {
				select {
				case packetCh <- pcmPacket{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	binState := make([]float64, NumBands) // smoothed output in [0,1]
	binNorm := make([]float64, NumBands)  // adaptive per-band reference
	fftIn := make([]float64, fftSize)
	fftPos := 0
	loudEnv := 0.0
	loudRef := 0.14
	lowBandEnv := 0.0
	bandRanges := buildBandRanges(fftSize, pcmSampleRate, NumBands, minFreqHz, maxFreqHz)
	window := hannWindow(fftSize)
	for i := range binNorm {
		binNorm[i] = 0.22
	}

	for {
		packet, ok := <-packetCh
		if !ok {
			return io.EOF
		}
		if len(packet.data) == 0 {
			if packet.err != nil {
				return packet.err
			}
			continue
		}

		chunk := packet.data
		n := len(chunk)
		if n == 0 {
			if packet.err != nil {
				return packet.err
			}
			continue
		}
		volScale := float64(clampVolume(p.Volume())) / 100.0
		fadeScale := float64(p.fadePermille.Load()) / 1000.0
		if fadeScale < 0 {
			fadeScale = 0
		}
		if fadeScale > 1 {
			fadeScale = 1
		}
		volScale *= fadeScale
		isPaused := atomic.LoadInt32(&p.pauseFlag) == 1
		playBuf := make([]byte, len(chunk))
		copy(playBuf, chunk)

		var out [NumBands]float64
		samplesFound := n / bytesPerSample
		rmsAccum := 0.0
		for i := range samplesFound {
			base := i * bytesPerSample
			sample := int16(binary.LittleEndian.Uint16(chunk[base:]))
			audioSample := sample
			if isPaused {
				audioSample = 0
			} else {
				audioSample = int16(float64(sample) * volScale)
			}
			binary.LittleEndian.PutUint16(playBuf[base:], uint16(audioSample))

			if i%pcmChannels == 0 {
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
						db := 20.0 * math.Log10(1e-9+bands[b])
						if db < -90 {
							db = -90
						}
						lin := (db + 90) / 90
						linVals[b] = lin
						linMean += lin
					}
					linMean /= float64(NumBands)

					frameVals := make([]float64, NumBands)
					frameMax := 0.0
					frameMean := 0.0
					lowNow := 0.0
					for b := range NumBands {
						lin := linVals[b]
						if lin > binNorm[b] {
							binNorm[b] += 0.045 * (lin - binNorm[b])
						} else {
							binNorm[b] *= 0.9993
						}
						autoGain := 1.0 / (0.24 + 1.9*binNorm[b])
						if autoGain > 1.45 {
							autoGain = 1.45
						}
						if autoGain < 0.55 {
							autoGain = 0.55
						}

						pos := float64(b) / float64(NumBands-1)
						bassBoost := 1.0 + 0.52*math.Exp(-5.5*pos)
						highLift := 0.92 + 0.34*math.Pow(pos, 1.15)

						v := lin * autoGain * bassBoost * highLift
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
						lowBandEnv += 0.45 * (lowNow - lowBandEnv)
					} else {
						lowBandEnv += 0.045 * (lowNow - lowBandEnv)
					}
					kickDelta := lowNow - lowBandEnv
					if kickDelta < 0 {
						kickDelta = 0
					}
					if kickDelta > 0.65 {
						kickDelta = 0.65
					}

					if frameMax > 1e-6 {
						targetPeak := 0.65 + 0.28*loudEnv
						if targetPeak > 0.95 {
							targetPeak = 0.95
						}
						scale := targetPeak / frameMax
						for b := range NumBands {
							v := frameVals[b]
							contrastFloor := frameMean * 0.42
							v = (v - contrastFloor) / (frameMax - contrastFloor + 1e-6)
							if v < 0 {
								v = 0
							}
							v = math.Pow(v, 1.15)
							v *= scale

							// Apply punchy bass boost
							if b < 7 {
								lowPos := 1.0 - float64(b)/7.0
								// Stronger kick impact
								v += kickDelta * (1.65 * lowPos)
							}

							// Boost dynamics for variety
							if v > 0 {
								v = math.Pow(v, 0.82)
							}

							if b >= NumBands/2 {
								highPos := float64(b-NumBands/2) / float64(NumBands/2)
								v += (0.012 + 0.025*loudEnv) * (0.45 + 0.55*highPos)
							}

							if v > 1.0 {
								v = 1.0
							}

							attack := 0.48
							decay := 0.82
							if b < 6 {
								attack = 0.65
								decay = 0.78
							}

							if v > binState[b] {
								binState[b] += attack * (v - binState[b])
							} else {
								binState[b] *= decay
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
					for b := range NumBands {
						out[b] = binState[b]
					}
				}
			}
		}

		chunkRMS := 0.0
		if samplesFound > 0 {
			chunkRMS = math.Sqrt(rmsAccum / float64(samplesFound))
		}
		if loudRef < 0.03 {
			loudRef = 0.03
		}
		if chunkRMS > loudRef {
			loudRef += 0.02 * (chunkRMS - loudRef)
		} else {
			loudRef += 0.002 * (chunkRMS - loudRef)
		}
		envTarget := chunkRMS / (loudRef * 1.22)
		if envTarget > 1 {
			envTarget = 1
		}
		if envTarget < 0 {
			envTarget = 0
		}
		if envTarget > loudEnv {
			loudEnv += 0.20 * (envTarget - loudEnv)
		} else {
			loudEnv += 0.45 * (envTarget - loudEnv)
		}
		loudGain := 0.05 + 0.95*loudEnv
		for b := range NumBands {
			out[b] *= loudGain
		}

		p.Viz.set(out)
		if _, err := audioOut.Write(playBuf); err != nil {
			return fmt.Errorf("write audio: %w", err)
		}
		p.recordAudioWrite(len(playBuf))
		if packet.err != nil {
			return packet.err
		}
	}
}

func hannWindow(n int) []float64 {
	w := make([]float64, n)
	if n <= 1 {
		return w
	}
	for i := range n {
		w[i] = 0.5 - 0.5*math.Cos((2*math.Pi*float64(i))/float64(n-1))
	}
	return w
}

func fftMagnitudes(samples []float64, window []float64) []float64 {
	n := len(samples)
	complexIn := make([]complex128, n)
	for i := range n {
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
	for b := range bands {
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
	audioOnce := p.audioOnce
	p.audioOnce = nil
	ctx := p.audioCtx
	p.audioCtx = nil
	p.ffmpegStderr = nil
	p.fadePermille.Store(1000)
	p.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}

	if cmd == nil {
		if player != nil {
			if audioOnce != nil {
				audioOnce.Do(func() { _ = player.Close() })
			} else {
				_ = player.Close()
			}
		}
		if ctx != nil {
			_ = ctx.Close()
		}
		p.Viz.set([NumBands]float64{})
		return nil
	}
	writeLog("playback.stopping")

	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if waitCh != nil {
		<-waitCh
	}
	if player != nil {
		if audioOnce != nil {
			audioOnce.Do(func() { _ = player.Close() })
		} else {
			_ = player.Close()
		}
	}
	if ctx != nil {
		_ = ctx.Close()
	}
	p.Viz.set([NumBands]float64{})
	writeLog("playback.stopped")
	return nil
}

func (p *Player) fadeOutAndStop(duration time.Duration) error {
	if duration <= 0 {
		return p.Stop()
	}

	if !p.IsRunning() {
		return p.Stop()
	}

	const steps = 18
	stepDur := duration / steps
	if stepDur <= 0 {
		stepDur = 15 * time.Millisecond
	}

	for i := steps - 1; i >= 0; i-- {
		if !p.IsRunning() {
			break
		}
		level := (i * 1000) / steps
		p.fadePermille.Store(int32(level))
		time.Sleep(stepDur)
	}
	p.fadePermille.Store(0)
	// Let the output buffer drain silence so process kill does not produce an audible cut.
	time.Sleep(140 * time.Millisecond)

	return p.Stop()
}

func (p *Player) fadeIn(duration time.Duration) {
	if duration <= 0 {
		p.fadePermille.Store(1000)
		return
	}
	if !p.IsRunning() {
		return
	}

	const steps = 14
	stepDur := duration / steps
	if stepDur <= 0 {
		stepDur = 20 * time.Millisecond
	}

	for i := 1; i <= steps; i++ {
		if !p.IsRunning() {
			return
		}
		level := (i * 1000) / steps
		if level > 1000 {
			level = 1000
		}
		p.fadePermille.Store(int32(level))
		time.Sleep(stepDur)
	}
	p.fadePermille.Store(1000)
}

func firstNonNilErr(primary error, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func isNaturalPlaybackEnd(pipelineErr, waitErr error) bool {
	if waitErr != nil {
		return false
	}
	if pipelineErr == nil {
		return true
	}
	return errors.Is(pipelineErr, io.EOF) || errors.Is(pipelineErr, io.ErrUnexpectedEOF)
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

func (p *Player) Stats() PlaybackStats {
	now := time.Now()
	p.statsMu.Lock()
	if p.rateSampleAt.IsZero() {
		p.rateSampleAt = now
		p.rateSampleBytes = p.bytesWritten
	}
	if elapsed := now.Sub(p.rateSampleAt); elapsed >= 250*time.Millisecond {
		deltaBytes := p.bytesWritten - p.rateSampleBytes
		instKbps := (float64(deltaBytes) * 8) / elapsed.Seconds() / 1000.0
		if instKbps < 0 {
			instKbps = 0
		}
		if p.rateKbps == 0 {
			p.rateKbps = instKbps
		} else {
			p.rateKbps = p.rateKbps*0.75 + instKbps*0.25
		}
		p.rateSampleAt = now
		p.rateSampleBytes = p.bytesWritten
	}
	kbps := p.rateKbps
	lastWriteUnix := p.lastWriteUnix
	p.statsMu.Unlock()

	stats := PlaybackStats{
		OutputKbps:    kbps,
		Reconnects:    p.reconnects.Load(),
		AnalyzerFresh: p.Viz.IsFresh(1200 * time.Millisecond),
		Running:       p.IsRunning(),
	}
	if lastWriteUnix > 0 {
		stats.LastWrite = time.Unix(0, lastWriteUnix)
	}
	if lastReconnect := p.lastReconnect.Load(); lastReconnect > 0 {
		stats.LastReconnect = time.Unix(0, lastReconnect)
	}
	return stats
}

func (p *Player) resetStats() {
	p.statsMu.Lock()
	p.bytesWritten = 0
	p.rateSampleBytes = 0
	p.rateSampleAt = time.Now()
	p.rateKbps = 0
	p.lastWriteUnix = 0
	p.statsMu.Unlock()
	p.reconnects.Store(0)
	p.lastReconnect.Store(0)
}

func (p *Player) recordAudioWrite(bytes int) {
	if bytes <= 0 {
		return
	}
	p.statsMu.Lock()
	p.bytesWritten += int64(bytes)
	p.lastWriteUnix = time.Now().UnixNano()
	p.statsMu.Unlock()
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
