//go:build !linux && !darwin && !windows

package recordfs

import (
	"errors"
	"os"
)

func lockFile(*os.File) error {
	return errors.New("recordfs: directory locking is unsupported on this platform")
}
func syncDirectory(string) error {
	return errors.New("recordfs: directory sync is unsupported on this platform")
}
