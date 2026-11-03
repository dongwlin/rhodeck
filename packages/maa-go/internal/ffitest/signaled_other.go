//go:build !unix

package ffitest

import "os/exec"

func exitSignaled(*exec.ExitError) bool { return false }
