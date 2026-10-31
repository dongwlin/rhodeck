package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// requireWorkerExited proves the fixture worker process is gone after the CLI
// reported cleanup. A Unix signal-0 probe reports whether the PID still exists;
// Windows cannot probe an unrelated process this way, so the direct liveness
// check is skipped there and the caller's cleanup assertions stand.
func requireWorkerExited(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("worker PID marker missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid worker PID marker %q", raw)
	}
	if runtime.GOOS == "windows" {
		t.Logf("worker %d liveness probe skipped on windows", pid)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("worker process %d was not reaped", pid)
	}
}
