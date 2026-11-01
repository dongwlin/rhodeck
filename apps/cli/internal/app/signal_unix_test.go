//go:build linux || darwin

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/stretchr/testify/require"
)

func TestSignalsConfirmStopAndReapWorker(t *testing.T) {
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			plan, executable, dir := testPlan(t)
			trace := filepath.Join(t.TempDir(), "commands")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, cliArgs("run", executable, dir)...)
			cmd.WaitDelay = 2 * time.Second
			cmd.Env = append(os.Environ(), "RHODECK_CLI_PROCESS=1", "RHODECK_CLI_WORKER_MODE=wait", "RHODECK_CLI_TRACE="+trace)
			cmd.Stdin = bytes.NewReader(encodePlan(t, plan))
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			// done releases the reader on failure paths. Without it a t.Fatal that
			// stops consuming lines leaves the goroutine blocked on a full channel
			// for the rest of the test binary.
			done := make(chan struct{})
			scanDone := make(chan struct{})
			lines := make(chan []byte, 32)
			var scanErr error
			go func() {
				defer close(scanDone)
				defer close(lines)
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					select {
					case lines <- bytes.Clone(scanner.Bytes()):
					case <-done:
						return
					}
				}
				scanErr = scanner.Err()
			}()
			t.Cleanup(func() {
				close(done)
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				<-scanDone
				if t.Failed() {
					t.Logf("CLI diagnostics: %s", diagnostics.String())
				}
			})
			sent := false
			var terminal envelope
		loop:
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						break loop
					}
					var event control.Event
					require.NoError(t, json.Unmarshal(line, &event), "invalid CLI output %q", line)
					if event.State == control.Started && !sent {
						require.NoError(t, cmd.Process.Signal(signal), "send %s", signal)
						sent = true
					}
					if event.Type == "terminal" {
						require.NoError(t, json.Unmarshal(line, &terminal), "invalid terminal envelope %q", line)
					}
				case <-ctx.Done():
					require.FailNow(t, "signal did not finish the CLI within its cleanup deadline")
				}
			}
			err = cmd.Wait()
			require.NoError(t, scanErr, "read CLI output: diagnostics=%s", diagnostics.String())
			require.NotNil(t, cmd.ProcessState, "CLI did not exit: %v diagnostics=%s", err, diagnostics.String())
			exit := cmd.ProcessState.ExitCode()
			require.True(t, sent, "signal result: sent=%t exit=%d terminal=%+v err=%v diagnostics=%s", sent, exit, terminal.Result, err, diagnostics.String())
			require.Equal(t, exitStopped, exit, "signal result: exit=%d terminal=%+v err=%v diagnostics=%s", exit, terminal.Result, err, diagnostics.String())
			require.NotNil(t, terminal.Result, "signal result: terminal=%+v err=%v diagnostics=%s", terminal.Result, err, diagnostics.String())
			require.Equal(t, control.Stopped, terminal.Result.State, "signal result: terminal=%+v err=%v diagnostics=%s", terminal.Result, err, diagnostics.String())
			require.Equal(t, "complete", terminal.Result.Cleanup, "signal result: terminal=%+v err=%v diagnostics=%s", terminal.Result, err, diagnostics.String())
			commands, err := os.ReadFile(trace)
			require.NoError(t, err, "worker was not stopped and closed")
			require.True(t, bytes.HasSuffix(commands, []byte("stop\nclose\n")), "worker was not stopped and closed: %s", commands)
		})
	}
}
