//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
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
			cmd := exec.Command(executable, cliArgs("run", executable, dir)...)
			cmd.Env = append(filteredCLIEnv(), "RHODECK_CLI_PROCESS=1", "RHODECK_CLI_WORKER_MODE=typedhold",
				"RHODECK_CLI_TRACE="+trace, "RHODECK_CLI_WORKER_PID="+pidFile)
			cmd.Stdin = bytes.NewReader(encodePlan(t, plan))
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			lines := make(chan []byte, 32)
			go func() {
				defer close(lines)
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					lines <- bytes.Clone(scanner.Bytes())
				}
			}()
			sent := false
			var terminal envelope
			deadline := time.After(15 * time.Second)
		loop:
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						break loop
					}
					var event control.Event
					_ = json.Unmarshal(line, &event)
					if event.Type == "typed" && !sent {
						if err := cmd.Process.Signal(signal); err != nil {
							t.Fatal(err)
						}
						sent = true
					}
					if event.Type == "terminal" {
						_ = json.Unmarshal(line, &terminal)
					}
				case <-deadline:
					t.Fatal("signal did not finish the CLI within its cleanup deadline")
				}
			}
			err = cmd.Wait()
			if !sent || cmd.ProcessState.ExitCode() != exitStopped || terminal.Result == nil ||
				terminal.Result.State != control.Stopped || terminal.Result.Cleanup != "complete" {
				t.Fatalf("signal result: sent=%t exit=%d terminal=%+v err=%v diagnostics=%s",
					sent, cmd.ProcessState.ExitCode(), terminal.Result, err, diagnostics.String())
			}
			if got := taskResult(t, terminal.Result.Tasks, "grow-1").Typed.Targets[0].Outcome; got != control.TargetAchieved {
				t.Fatalf("signal lost the confirmed fact: %q", got)
			}
			commands, err := os.ReadFile(trace)
			if err != nil || !bytes.HasSuffix(commands, []byte("stop\nclose\n")) {
				t.Fatalf("worker was not stopped and closed: %s (%v)", commands, err)
			}
			requireWorkerExited(t, pidFile)
		})
	}
}
