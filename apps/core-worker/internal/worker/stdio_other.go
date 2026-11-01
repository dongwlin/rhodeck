//go:build !linux && !darwin && !windows

package worker

import (
	"errors"
	"os"
)

func isolateStdout() (*os.File, error) { return nil, errors.New("unsupported worker platform") }
