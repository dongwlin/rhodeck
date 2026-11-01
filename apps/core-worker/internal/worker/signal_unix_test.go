//go:build linux || darwin

package worker

import (
	"bytes"
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/stretchr/testify/require"
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
			require.NoError(t, err)
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			_, err = protocol.NewReader(stdout).Read()
			require.NoError(t, err)
			require.NoError(t, cmd.Process.Signal(signal))
			var exit *exec.ExitError
			err = cmd.Wait()
			require.ErrorAs(t, err, &exit, "signal shutdown; diagnostics: %s", diagnostics.String())
			require.Equal(t, 1, exit.ExitCode(), "signal shutdown; diagnostics: %s", diagnostics.String())
			require.NoError(t, ctx.Err(), "signal shutdown; diagnostics: %s", diagnostics.String())
		})
	}
}
