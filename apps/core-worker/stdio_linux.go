//go:build linux

package main

import "syscall"

func redirectDescriptor(from, to int) error { return syscall.Dup3(from, to, 0) }
