package radio

import (
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/utils"
)

func runYtDlp(args ...string) (string, string, error) {
	return utils.RunCommand(config.YtDlpPath(), args...)
}
