//go:build !darwin

package apple

import (
	"errors"

	"github.com/eastsidegunn/oaudio/runner"
)

// New fails on platforms other than macOS.
func New(runner.Model) (runner.Engine, error) {
	return nil, errors.New("the apple engine only runs on macOS")
}
