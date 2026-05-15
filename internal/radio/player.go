package radio

import (
	"errors"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/kidixdev/lofi-radio/internal/config"
)

var errPlayerNotRunning = errors.New("player is not running")

type Player struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	waitCh chan error
	volume int
}

func NewPlayer(initialVolume int) *Player {
	return &Player{
		volume: clampVolume(initialVolume),
	}
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
		return err
	}

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
	p.mu.Unlock()

	return nil
}

func (p *Player) Stop() error {
	p.mu.Lock()
	cmd := p.cmd
	stdinPipe := p.stdin
	waitCh := p.waitCh

	p.cmd = nil
	p.stdin = nil
	p.waitCh = nil
	p.mu.Unlock()

	if cmd == nil {
		return nil
	}

	if stdinPipe != nil {
		_, _ = stdinPipe.Write([]byte{'q'})
		_ = stdinPipe.Close()
	}

	if waitCh != nil {
		select {
		case <-waitCh:
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
	currentVolume := p.volume
	p.mu.Unlock()

	err := p.sendKey('0')
	return currentVolume, err
}

func (p *Player) DecreaseVolume(step int) (int, error) {
	if step <= 0 {
		step = 5
	}

	p.mu.Lock()
	p.volume = clampVolume(p.volume - step)
	currentVolume := p.volume
	p.mu.Unlock()

	err := p.sendKey('9')
	return currentVolume, err
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
