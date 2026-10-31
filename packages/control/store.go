package control

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const maxRecordBytes = MaxPlanBytes + (1 << 20)

var runIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type store struct {
	mu   sync.Mutex
	dir  string
	lock *os.File
}

func newRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func openStore(dir string) (*store, error) {
	if !validPath(dir, 65536) {
		return nil, errors.New("control: data directory must be absolute")
	}
	if err := makePrivateDirectory(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	s := &store{dir: filepath.Join(dir, "runs"), lock: f}
	if err = makePrivateDirectory(s.dir); err == nil {
		err = os.Chmod(s.dir, 0700)
	}
	if err == nil {
		err = s.recover()
	}
	if err != nil {
		_ = s.close()
		return nil, err
	}
	return s, nil
}

// recover changes only unconfirmed records, without launching a worker. A host
// crash does not establish whether an old device action completed or stopped.
func (s *store) recover() error {
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
			name := entry.Name()
			if strings.HasPrefix(name, ".record-") {
				if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
					return err
				}
				continue
			}
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if !runIDPattern.MatchString(id) || !entry.Type().IsRegular() {
				return errors.New("control: invalid record entry")
			}
			record, err := ReadRecord(filepath.Dir(s.dir), id)
			if err != nil {
				return err
			}
			if terminal(record.Result.State) && (record.Result.Cleanup == "complete" || record.Result.Cleanup == "failed" || record.Result.Code == "host_interrupted") {
				continue
			}
			if !terminal(record.Result.State) {
				record.Result.State = Unknown
			}
			record.Result.Code = "host_interrupted"
			record.Result.Cleanup = "unconfirmed"
			record.Result.CleanupCode = "host_interrupted"
			record.Result.FinishedAt = time.Now().UTC()
			for i := range record.Result.Tasks {
				if !terminal(record.Result.Tasks[i].State) {
					record.Result.Tasks[i].State = Unknown
				}
			}
			if err := s.save(record); err != nil {
				return fmt.Errorf("%w: %w", ErrRecordWrite, err)
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	return nil
}

// Persist newly created directory names before a record may authorize device
// work. Syncing only the final file's directory would not persist its parent.
func makePrivateDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return errors.New("control: expected directory")
		}
		// Complete a preceding creator's interrupted directory sync as well.
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

func terminal(s State) bool {
	return s == Succeeded || s == Failed || s == Stopped || s == Rejected || s == Unknown
}

func (s *store) save(record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrClosed
	}
	if err := validateRecord(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > maxRecordBytes {
		return errors.New("control: record exceeds byte limit")
	}
	f, err := os.CreateTemp(s.dir, ".record-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
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
	if err = os.Rename(name, filepath.Join(s.dir, record.Result.RunID+".json")); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *store) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close() // OS ownership is released even after an abrupt process exit.
	s.lock = nil
	return err
}

// ReadRecord reads a bounded private snapshot without acquiring runtime
// ownership. Replacement is atomic, so readers observe an old or new record.
func ReadRecord(dataDir, runID string) (Record, error) {
	var record Record
	if !validPath(dataDir, 65536) || !runIDPattern.MatchString(runID) {
		return record, errors.New("control: invalid record path")
	}
	f, err := os.Open(filepath.Join(dataDir, "runs", runID+".json"))
	if err != nil {
		return record, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return record, err
	}
	if len(raw) > maxRecordBytes {
		return record, errors.New("control: record exceeds byte limit")
	}
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := checkControlKeys(keys); err != nil {
		return Record{}, errors.New("control: ambiguous record fields")
	}
	if err = protocol.Decode(raw, &record); err != nil {
		return record, fmt.Errorf("control: invalid record: %w", err)
	}
	if record.Result.RunID != runID {
		return Record{}, errors.New("control: record identity or version mismatch")
	}
	if err := validateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Records require complete plan/result associations without loading an adapter
// or installation. Recovery only preserves facts; it never prepares or resumes
// device work. Unknown data formats are rejected instead of migrated.
func validateRecord(record Record) error {
	if record.SchemaVersion != SchemaVersion || record.Result.SchemaVersion != SchemaVersion ||
		record.Plan.SchemaVersion != SchemaVersion || !runIDPattern.MatchString(record.Result.RunID) {
		return errors.New("control: record identity or version mismatch")
	}
	if err := ValidatePlan(record.Plan); err != nil {
		return fmt.Errorf("control: invalid record plan: %w", err)
	}
	if len(record.Plan.Tasks) != len(record.Result.Tasks) {
		return errors.New("control: record task association mismatch")
	}
	for i, task := range record.Plan.Tasks {
		result := record.Result.Tasks[i]
		if task.ID != result.ID || (task.Adapter == nil) != (result.Typed == nil) {
			return errors.New("control: record task association mismatch")
		}
		if task.Adapter != nil && *task.Adapter != result.Typed.Adapter {
			return errors.New("control: record adapter association mismatch")
		}
		if record.Result.State == Succeeded && (result.State != Succeeded || (result.Typed != nil && result.Typed.State != Succeeded)) {
			return errors.New("control: record success lacks confirmed task facts")
		}
	}
	return validateTypedResults(record.Result.Tasks)
}
