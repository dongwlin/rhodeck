//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func TestWorkerSignalExit(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, workerBinary(t))
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			if _, err := protocol.NewReader(stdout).Read(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			var exit *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 1 || ctx.Err() != nil {
				t.Fatalf("signal shutdown: %v, %v; diagnostics: %s", err, ctx.Err(), diagnostics.String())
			}
		})
	}
}
