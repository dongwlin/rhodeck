package control

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"

	"github.com/dongwlin/rhodeck/packages/control/internal/recordfs"
)

type store struct {
	mu     sync.Mutex
	dir    string
	files  *recordfs.Store
	closed bool
}

func openStore(dir string) (*store, error) {
	if !validPath(dir, 65536) {
		return nil, errors.New("control: data directory must be absolute")
	}
	files, err := recordfs.Open(dir)
	if err != nil {
		return nil, recordFSError(err)
	}
	s := &store{dir: filepath.Join(dir, "runs"), files: files}
	if err := s.recover(); err != nil {
		_ = s.close()
		return nil, err
	}
	return s, nil
}

func (s *store) save(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := validateRecord(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return recordFSError(s.files.Write(record.Result.RunID+".json", raw, maxRecordBytes))
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	err := s.files.Close()
	s.closed = true
	return recordFSError(err)
}

func recordFSError(err error) error {
	switch {
	case errors.Is(err, recordfs.ErrBusy):
		return ErrBusy
	case errors.Is(err, recordfs.ErrClosed):
		return ErrClosed
	case errors.Is(err, recordfs.ErrTooLarge):
		return errors.New("control: record exceeds byte limit")
	default:
		return err
	}
}
