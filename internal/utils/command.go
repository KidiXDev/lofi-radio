package utils

import (
	"bytes"
	"os/exec"
	"strings"
)

func RunCommand(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}
