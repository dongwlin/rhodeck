//go:build darwin

package app

import "syscall"

func redirectDescriptor(from, to int) error { return syscall.Dup2(from, to) }
