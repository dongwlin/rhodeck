package recordfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOwnershipAndClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if second, err := Open(dir); !errors.Is(err, ErrBusy) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second ownership = %v, want ErrBusy", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if err := s.Write("run.json", []byte("{}"), 2); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v, want ErrClosed", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime.lock")); err != nil {
		t.Fatalf("lock inode was removed: %v", err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("ownership after close: %v", err)
	}
	activeTemp := filepath.Join(dir, "runs", ".record-current-owner")
	if err := os.WriteFile(activeTemp, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Scan(func(fs.DirEntry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(activeTemp); err != nil {
		t.Fatalf("closed store removed another owner's temporary file: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementAndBoundedRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private", "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, raw := range [][]byte{[]byte("original"), []byte("new")} {
		if err := s.Write("run.json", raw, 8); err != nil {
			t.Fatal(err)
		}
		got, err := Read(dir, "run.json", len(raw))
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("read replacement = %q, %v; want %q", got, err, raw)
		}
	}
	if _, err := Read(dir, "run.json", 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("bounded read = %v, want ErrTooLarge", err)
	}
	if err := s.Write("run.json", []byte("oversized"), 8); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("bounded write = %v, want ErrTooLarge", err)
	}
	got, err := Read(dir, "run.json", 8)
	if err != nil || string(got) != "new" {
		t.Fatalf("failed write changed record = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		for name, mode := range map[string]fs.FileMode{"runs": 0700, "runs/run.json": 0600, "runtime.lock": 0600} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("permissions for %s = %v, %v; want %v", name, info, err, mode)
			}
		}
	}
}

func TestFailedReplacementCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Mkdir(filepath.Join(dir, "runs", "blocked.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Write("blocked.json", []byte("{}"), 2); err == nil {
		t.Fatal("replaced a directory with a record")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "blocked.json" || !entries[0].IsDir() {
		t.Fatalf("failed replacement left temporary files: %v", entries)
	}
}

func TestScanBatchesAndCleansAbandonedWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 130 {
		if err := s.Write(fmt.Sprintf("%03d.json", i), []byte("{}"), 2); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "runs", ".record-abandoned"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	if err := s.Scan(func(entry fs.DirEntry) error {
		if seen[entry.Name()] || strings.HasPrefix(entry.Name(), ".record-") {
			return fmt.Errorf("unexpected entry %q", entry.Name())
		}
		seen[entry.Name()] = true
		// Recovery may replace the same record inside the scan visitor.
		return s.Write(entry.Name(), []byte("[]"), 2)
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 130 {
		t.Fatalf("scanned %d records, want 130", len(seen))
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", ".record-abandoned")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned temporary file survived: %v", err)
	}
	visitorErr := errors.New("visitor failed")
	if err := s.Scan(func(fs.DirEntry) error { return visitorErr }); !errors.Is(err, visitorErr) {
		t.Fatalf("visitor error = %v, want %v", err, visitorErr)
	}
}

func TestRejectsUnsafeNamesAndInvalidLimits(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"", ".", "..", "../escape", `..\escape`, "/absolute", `C:escape`, "name:stream", "name\x00", string([]byte{0xff}), ".record-reserved"} {
		t.Run(fmt.Sprintf("name_%q", name), func(t *testing.T) {
			if err := s.Write(name, nil, 1); err == nil {
				t.Fatal("accepted unsafe write name")
			}
			if _, err := Read(dir, name, 1); err == nil {
				t.Fatal("accepted unsafe read name")
			}
		})
	}
	limits := []int{-1, 0}
	if maxInt := int(^uint(0) >> 1); uint64(maxInt) >= math.MaxInt64 {
		limits = append(limits, maxInt)
	}
	for _, limit := range limits {
		if err := s.Write("run.json", nil, limit); err == nil {
			t.Fatalf("accepted write limit %d", limit)
		}
		if _, err := Read(dir, "run.json", limit); err == nil {
			t.Fatalf("accepted read limit %d", limit)
		}
	}
	if _, err := Open("relative"); err == nil {
		t.Fatal("accepted relative directory")
	}
	if err := s.Scan(nil); err == nil {
		t.Fatal("accepted missing visitor")
	}
}
