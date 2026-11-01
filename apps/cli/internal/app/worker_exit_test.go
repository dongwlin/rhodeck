package app

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireWorkerExited proves the fixture worker process is gone after the CLI
// reported cleanup. A Unix signal-0 probe reports whether the PID still exists;
// Windows cannot probe an unrelated process this way, so the direct liveness
// check is skipped there and the caller's cleanup assertions stand.
func requireWorkerExited(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "worker PID marker missing")
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err, "invalid worker PID marker %q", raw)
	require.True(t, pid > 0, "invalid worker PID marker %q", raw)
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
