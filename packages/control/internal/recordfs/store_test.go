package recordfs

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnershipAndClose(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	if second, err := Open(dir); !errors.Is(err, ErrBusy) {
		if second != nil {
			_ = second.Close()
		}
		require.ErrorIs(t, err, ErrBusy, "second ownership want ErrBusy")
	}
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "repeated close")
	require.ErrorIs(t, s.Write("run.json", []byte("{}"), 2), ErrClosed, "write after close want ErrClosed")
	_, err = os.Stat(filepath.Join(dir, "runtime.lock"))
	require.NoError(t, err, "lock inode was removed")
	second, err := Open(dir)
	require.NoError(t, err, "ownership after close")
	activeTemp := filepath.Join(dir, "runs", ".record-current-owner")
	require.NoError(t, os.WriteFile(activeTemp, []byte("partial"), 0600))
	require.NoError(t, s.Scan(func(fs.DirEntry) error { return nil }))
	_, err = os.Stat(activeTemp)
	require.NoError(t, err, "closed store removed another owner's temporary file")
	require.NoError(t, second.Close())
}

func TestReplacementAndBoundedRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private", "data")
	s, err := Open(dir)
	require.NoError(t, err)
	defer s.Close()
	for _, raw := range [][]byte{[]byte("original"), []byte("new")} {
		require.NoError(t, s.Write("run.json", raw, 8))
		got, err := Read(dir, "run.json", len(raw))
		require.NoError(t, err, "read replacement")
		require.Equal(t, raw, got, "read replacement")
	}
	_, err = Read(dir, "run.json", 2)
	require.ErrorIs(t, err, ErrTooLarge, "bounded read want ErrTooLarge")
	require.ErrorIs(t, s.Write("run.json", []byte("oversized"), 8), ErrTooLarge, "bounded write want ErrTooLarge")
	got, err := Read(dir, "run.json", 8)
	require.NoError(t, err, "failed write changed record")
	require.Equal(t, "new", string(got), "failed write changed record")
	if runtime.GOOS != "windows" {
		for name, mode := range map[string]fs.FileMode{"runs": 0700, "runs/run.json": 0600, "runtime.lock": 0600} {
			info, err := os.Stat(filepath.Join(dir, name))
			require.NoError(t, err, "permissions for %s", name)
			require.Equal(t, mode, info.Mode().Perm(), "permissions for %s", name)
		}
	}
}

func TestFailedReplacementCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "runs", "blocked.json"), 0700))
	require.Error(t, s.Write("blocked.json", []byte("{}"), 2), "replaced a directory with a record")
	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "failed replacement left temporary files")
	require.Equal(t, "blocked.json", entries[0].Name())
	require.True(t, entries[0].IsDir())
}

func TestScanBatchesAndCleansAbandonedWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	defer s.Close()
	for i := range 130 {
		require.NoError(t, s.Write(fmt.Sprintf("%03d.json", i), []byte("{}"), 2))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "runs", ".record-abandoned"), []byte("partial"), 0600))
	seen := make(map[string]bool)
	require.NoError(t, s.Scan(func(entry fs.DirEntry) error {
		if seen[entry.Name()] || strings.HasPrefix(entry.Name(), ".record-") {
			return fmt.Errorf("unexpected entry %q", entry.Name())
		}
		seen[entry.Name()] = true
		// Recovery may replace the same record inside the scan visitor.
		return s.Write(entry.Name(), []byte("[]"), 2)
	}))
	require.Equal(t, 130, len(seen), "scanned %d records", len(seen))
	_, err = os.Stat(filepath.Join(dir, "runs", ".record-abandoned"))
	require.ErrorIs(t, err, os.ErrNotExist, "abandoned temporary file survived")
	visitorErr := errors.New("visitor failed")
	require.ErrorIs(t, s.Scan(func(fs.DirEntry) error { return visitorErr }), visitorErr, "visitor error")
}

func TestRejectsUnsafeNamesAndInvalidLimits(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	require.NoError(t, err)
	defer s.Close()
	for _, name := range []string{"", ".", "..", "../escape", `..\escape`, "/absolute", `C:escape`, "name:stream", "name\x00", string([]byte{0xff}), ".record-reserved"} {
		t.Run(fmt.Sprintf("name_%q", name), func(t *testing.T) {
			require.Error(t, s.Write(name, nil, 1), "accepted unsafe write name")
			_, err := Read(dir, name, 1)
			require.Error(t, err, "accepted unsafe read name")
		})
	}
	limits := []int{-1, 0}
	if maxInt := int(^uint(0) >> 1); uint64(maxInt) >= math.MaxInt64 {
		limits = append(limits, maxInt)
	}
	for _, limit := range limits {
		require.Error(t, s.Write("run.json", nil, limit), "accepted write limit %d", limit)
		_, err := Read(dir, "run.json", limit)
		require.Error(t, err, "accepted read limit %d", limit)
	}
	_, err = Open("relative")
	require.Error(t, err, "accepted relative directory")
	require.Error(t, s.Scan(nil), "accepted missing visitor")
}
