// Covers: record.go, store.go and recovery.go.

package control

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreRecoversWithoutReplaying(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	require.NoError(t, err)
	id := newRunID()
	plan := validTestPlan(t)
	plan.Tasks[0].ID = "done"
	pending := plan.Tasks[0]
	pending.ID = "pending"
	plan.Tasks = append(plan.Tasks, pending)
	rec := Record{SchemaVersion: SchemaVersion, Plan: plan, Result: Result{SchemaVersion: SchemaVersion, RunID: id, State: Started, Cleanup: "pending", Tasks: []TaskResult{{ID: "done", State: Succeeded}, {ID: "pending", State: Started}}}}
	require.NoError(t, s.save(rec))
	require.NoError(t, s.close())
	s, err = openStore(dir)
	require.NoError(t, err)
	defer s.close()
	got, err := ReadRecord(dir, id)
	require.NoError(t, err)
	require.True(t, got.Result.State == Unknown && got.Result.Code == "host_interrupted" && got.Result.Cleanup == "unconfirmed" &&
		got.Result.Tasks[0].State == Succeeded && got.Result.Tasks[1].State == Unknown, "incorrect recovered facts: %+v", got.Result)
	require.True(t, strings.Contains(string(got.Plan.Tasks[0].Params), "9007199254740993"), "record lost integer precision")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "runs", id+".json"))
		require.True(t, err == nil && info.Mode().Perm() == 0600, "private record permissions: %v %v", info, err)
	}
}

func TestStoreLockReleasedAfterProcessExit(t *testing.T) {
	if os.Getenv("RHODECK_TEST_STORE_LOCK") == "1" {
		s, err := openStore(os.Getenv("RHODECK_TEST_STORE_DIR"))
		if err != nil {
			os.Exit(11)
		}
		_ = s // Exit deliberately skips close: the OS must release ownership.
		_, _ = os.Stdout.Write([]byte("ready\n"))
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreLockReleasedAfterProcessExit$")
	cmd.Env = append(os.Environ(), "RHODECK_TEST_STORE_LOCK=1", "RHODECK_TEST_STORE_DIR="+dir)
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan error, 1)
	go func() {
		b := make([]byte, 6)
		_, err := output.Read(b)
		if err == nil && string(b) != "ready\n" {
			err = errors.New("invalid helper readiness")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "helper did not acquire lock")
	}
	second, err := openStore(dir)
	if second != nil {
		_ = second.close()
	}
	require.ErrorIs(t, err, ErrBusy, "expected process lock, got %v", err)
	_ = input.Close()
	require.NoError(t, cmd.Wait())
	s, err := openStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.close())
	// The lock inode stays in place: unlinking it would permit lock bypass.
	_, err = os.Stat(filepath.Join(dir, "runtime.lock"))
	require.NoError(t, err)
}

func TestStoreRejectsCorruptionAndTraversal(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	require.NoError(t, err)
	require.Error(t, s.save(Record{SchemaVersion: SchemaVersion, Result: Result{SchemaVersion: SchemaVersion, RunID: "../escape"}}), "accepted unsafe record identity")
	_, err = ReadRecord(dir, "../escape")
	require.Error(t, err, "accepted traversal")
	_ = s.close()
	broken := Record{SchemaVersion: SchemaVersion + 1, Result: Result{SchemaVersion: SchemaVersion, RunID: newRunID()}}
	raw, _ := json.Marshal(broken)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "runs", broken.Result.RunID+".json"), raw, 0600))
	s, err = openStore(dir)
	if s != nil {
		_ = s.close()
	}
	require.Error(t, err, "accepted unsupported record schema")
}

// A recovery resave must be classifiable as a record persistence failure rather
// than a generic startup error. Closing the store first makes the resave return
// ErrClosed without relying on filesystem permissions.
func TestRecoverySaveFailureWrapsErrRecordWrite(t *testing.T) {
	s, err := openStore(t.TempDir())
	require.NoError(t, err)
	record := typedStoreRecord(t)
	require.NoError(t, s.save(record))
	require.NoError(t, s.close())
	err = s.recover()
	require.ErrorIs(t, err, ErrRecordWrite, "recovery save error = %v, want ErrRecordWrite", err)
	require.ErrorIs(t, err, ErrClosed, "recovery save lost the underlying ErrClosed: %v", err)
}
