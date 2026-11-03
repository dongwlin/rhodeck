// Package recordfs owns private run-record files and the data-directory lock.
// It treats record contents as bounded bytes and has no knowledge of run states.
package recordfs

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

var (
	ErrBusy     = errors.New("recordfs: data directory is busy")
	ErrClosed   = errors.New("recordfs: store is closed")
	ErrTooLarge = errors.New("recordfs: record exceeds byte limit")
)

// Store owns one data-directory lock and serializes file replacements.
type Store struct {
	mu   sync.Mutex
	dir  string
	lock *os.File
}

// Open acquires ownership and prepares private directories without interpreting
// or recovering any records. Only Close releases ownership in a live process.
func Open(dataDir string) (*Store, error) {
	if !validDirectory(dataDir) {
		return nil, errors.New("recordfs: data directory must be absolute")
	}
	if err := makePrivateDirectory(dataDir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	s := &Store{dir: filepath.Join(dataDir, "runs"), lock: f}
	if err = makePrivateDirectory(s.dir); err == nil {
		err = os.Chmod(s.dir, 0700)
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Scan reads directory entries in bounded batches and removes abandoned write
// files while ownership is held. The callback may read or replace records. Like
// Read, enumeration does not require live ownership; writes fail after Close.
func (s *Store) Scan(visit func(fs.DirEntry) error) error {
	if visit == nil {
		return errors.New("recordfs: scan requires a visitor")
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".record-") {
				s.mu.Lock()
				var err error
				if s.lock != nil {
					err = os.Remove(filepath.Join(s.dir, entry.Name()))
				}
				s.mu.Unlock()
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

// Write flushes a private temporary file, atomically replaces name, and syncs
// directory metadata where the platform supports it. Failed writes clean up
// their temporary files. Names must be individual filenames, never paths.
func (s *Store) Write(name string, raw []byte, maxBytes int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	if err := validateInput(name, maxBytes); err != nil {
		return err
	}
	if len(raw) > maxBytes {
		return ErrTooLarge
	}
	f, err := os.CreateTemp(s.dir, ".record-")
	if err != nil {
		return err
	}
	tempName := f.Name()
	defer os.Remove(tempName)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tempName, filepath.Join(s.dir, name)); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

// Close releases ownership without unlinking the lock inode. Repeated calls
// succeed; the operating system also releases the lock after abrupt exit.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// Read returns a bounded atomic snapshot without acquiring execution ownership.
func Read(dataDir, name string, maxBytes int) ([]byte, error) {
	if !validDirectory(dataDir) {
		return nil, errors.New("recordfs: data directory must be absolute")
	}
	if err := validateInput(name, maxBytes); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dataDir, "runs", name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return nil, ErrTooLarge
	}
	return raw, nil
}

func validateInput(name string, maxBytes int) error {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".record-") || strings.ContainsAny(name, "/\\:\x00") || !utf8.ValidString(name) {
		return errors.New("recordfs: invalid record filename")
	}
	if maxBytes <= 0 || uint64(maxBytes) >= math.MaxInt64 {
		return errors.New("recordfs: invalid record byte limit")
	}
	return nil
}

func validDirectory(path string) bool {
	return filepath.IsAbs(path) && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}

func makePrivateDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("recordfs: expected directory")
		}
		return syncDirectory(filepath.Dir(path))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := makePrivateDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDirectory(parent)
}
