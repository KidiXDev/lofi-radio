package radio

import (
	"github.com/kidixdev/lofi-radio/internal/config"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func PlayStream(streamURL string) error {
	cmd := exec.Command(
		config.FFplayPath(),
		"-nodisp",
		"-autoexit",
		"-loglevel", "warning",
		streamURL,
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop

		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}

		os.Exit(0)
	}()

	return cmd.Wait()
}
