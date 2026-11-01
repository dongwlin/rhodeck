package control

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireWorkerProcessExited proves the fixture worker process is gone after the
// runtime reported Done and cleanup. A Unix signal-0 probe reports whether the
// PID still exists; Windows cannot probe an unrelated process this way, so the
// direct liveness check is skipped there and the runtime cleanup assertions
// stand.
func requireWorkerProcessExited(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "worker PID marker missing: %v", err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err, "invalid worker PID marker %q", raw)
	require.Greater(t, pid, 0, "invalid worker PID marker %q", raw)
	if runtime.GOOS == "windows" {
		t.Logf("worker %d liveness probe skipped on windows", pid)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	require.Error(t, proc.Signal(syscall.Signal(0)), "worker process %d was not reaped", pid)
}
