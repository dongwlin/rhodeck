//go:build linux || darwin

package cli

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
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
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
					if err := json.Unmarshal(line, &event); err != nil {
						t.Fatalf("invalid CLI output %q: %v", line, err)
					}
					if event.State == control.Started && !sent {
						if err := cmd.Process.Signal(signal); err != nil {
							t.Fatalf("send %s: %v", signal, err)
						}
						sent = true
					}
					if event.Type == "terminal" {
						if err := json.Unmarshal(line, &terminal); err != nil {
							t.Fatalf("invalid terminal envelope %q: %v", line, err)
						}
					}
				case <-ctx.Done():
					t.Fatal("signal did not finish the CLI within its cleanup deadline")
				}
			}
			err = cmd.Wait()
			if scanErr != nil {
				t.Fatalf("read CLI output: %v diagnostics=%s", scanErr, diagnostics.String())
			}
			if cmd.ProcessState == nil {
				t.Fatalf("CLI did not exit: %v diagnostics=%s", err, diagnostics.String())
			}
			exit := cmd.ProcessState.ExitCode()
			if !sent || exit != exitStopped || terminal.Result == nil || terminal.Result.State != control.Stopped || terminal.Result.Cleanup != "complete" {
				t.Fatalf("signal result: sent=%t exit=%d terminal=%+v err=%v diagnostics=%s", sent, exit, terminal.Result, err, diagnostics.String())
			}
			commands, err := os.ReadFile(trace)
			if err != nil || !bytes.HasSuffix(commands, []byte("stop\nclose\n")) {
				t.Fatalf("worker was not stopped and closed: %s (%v)", commands, err)
			}
		})
	}
}
