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
)

func TestStoreRecoversWithoutReplaying(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := newRunID()
	plan := validTestPlan(t)
	plan.Tasks[0].ID = "done"
	pending := plan.Tasks[0]
	pending.ID = "pending"
	plan.Tasks = append(plan.Tasks, pending)
	rec := Record{SchemaVersion: SchemaVersion, Plan: plan, Result: Result{SchemaVersion: SchemaVersion, RunID: id, State: Started, Cleanup: "pending", Tasks: []TaskResult{{ID: "done", State: Succeeded}, {ID: "pending", State: Started}}}}
	if err := s.save(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	got, err := ReadRecord(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result.State != Unknown || got.Result.Code != "host_interrupted" || got.Result.Cleanup != "unconfirmed" || got.Result.Tasks[0].State != Succeeded || got.Result.Tasks[1].State != Unknown {
		t.Fatalf("incorrect recovered facts: %+v", got.Result)
	}
	if !strings.Contains(string(got.Plan.Tasks[0].Params), "9007199254740993") {
		t.Fatal("record lost integer precision")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "runs", id+".json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private record permissions: %v %v", info, err)
		}
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
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper did not acquire lock")
	}
	if second, err := openStore(dir); !errors.Is(err, ErrBusy) {
		if second != nil {
			_ = second.close()
		}
		t.Fatalf("expected process lock, got %v", err)
	}
	_ = input.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	// The lock inode stays in place: unlinking it would permit lock bypass.
	if _, err := os.Stat(filepath.Join(dir, "runtime.lock")); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsCorruptionAndTraversal(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(Record{SchemaVersion: SchemaVersion, Result: Result{SchemaVersion: SchemaVersion, RunID: "../escape"}}); err == nil {
		t.Fatal("accepted unsafe record identity")
	}
	if _, err := ReadRecord(dir, "../escape"); err == nil {
		t.Fatal("accepted traversal")
	}
	_ = s.close()
	broken := Record{SchemaVersion: SchemaVersion + 1, Result: Result{SchemaVersion: SchemaVersion, RunID: newRunID()}}
	raw, _ := json.Marshal(broken)
	if err := os.WriteFile(filepath.Join(dir, "runs", broken.Result.RunID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(dir); err == nil {
		_ = s.close()
		t.Fatal("accepted unsupported record schema")
	}
}
