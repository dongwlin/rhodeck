//go:build linux || darwin

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/stretchr/testify/require"
)

// SIGINT and SIGTERM request the same graceful stop as context cancellation.
// A confirmed typed fact survives the signal, cleanup completes and the exit
// code reports a stop rather than a success or an unknown failure.
func TestTypedSignalStopKeepsConfirmedFacts(t *testing.T) {
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			plan, executable, dir := typedPlan(t,
				operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
			)
			trace := filepath.Join(t.TempDir(), "commands")
			pidFile := filepath.Join(t.TempDir(), "worker.pid")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, cliArgs("run", executable, dir)...)
			cmd.WaitDelay = 2 * time.Second
			cmd.Env = append(filteredCLIEnv(), "RHODECK_CLI_PROCESS=1", "RHODECK_CLI_WORKER_MODE=typedhold",
				"RHODECK_CLI_TRACE="+trace, "RHODECK_CLI_WORKER_PID="+pidFile)
			cmd.Stdin = bytes.NewReader(encodePlan(t, plan))
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			// Stop the scanner and reap the CLI even when an assertion ends the
			// test before all output has been consumed.
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
			var terminal output.Terminal
		loop:
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						break loop
					}
					var event control.Event
					require.NoError(t, json.Unmarshal(line, &event), "invalid CLI output %q", line)
					if event.Type == "typed" && !sent {
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
			require.True(t, sent, "signal result: sent=%t exit=%d terminal=%+v err=%v diagnostics=%s",
				sent, exit, terminal.Result, err, diagnostics.String())
			require.Equal(t, output.ExitStopped, exit, "signal result: exit=%d terminal=%+v err=%v diagnostics=%s",
				exit, terminal.Result, err, diagnostics.String())
			require.NotNil(t, terminal.Result, "signal result: terminal=%+v err=%v diagnostics=%s",
				terminal.Result, err, diagnostics.String())
			require.Equal(t, control.Stopped, terminal.Result.State, "signal result: terminal=%+v err=%v diagnostics=%s",
				terminal.Result, err, diagnostics.String())
			require.Equal(t, "complete", terminal.Result.Cleanup, "signal result: terminal=%+v err=%v diagnostics=%s",
				terminal.Result, err, diagnostics.String())
			require.Equal(t, control.TargetAchieved, taskResult(t, terminal.Result.Tasks, "grow-1").Typed.Targets[0].Outcome,
				"signal lost the confirmed fact")
			commands, err := os.ReadFile(trace)
			require.NoError(t, err, "worker was not stopped and closed")
			require.True(t, bytes.HasSuffix(commands, []byte("stop\nclose\n")), "worker was not stopped and closed: %s", commands)
			requireWorkerExited(t, pidFile)
		})
	}
}
