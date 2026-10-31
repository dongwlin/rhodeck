//go:build !linux && !darwin && !windows

package control

import (
	"errors"
	"os"
)

func lockFile(*os.File) error {
	return errors.New("control: directory locking is unsupported on this platform")
}
func syncDirectory(string) error {
	return errors.New("control: directory sync is unsupported on this platform")
}
