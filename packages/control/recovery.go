package control

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// recover changes only unconfirmed records, without launching a worker. A host
// crash does not establish whether an old device action completed or stopped.
func (s *store) recover() error {
	return s.files.Scan(func(entry fs.DirEntry) error {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			return nil
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
			return nil
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
		return nil
	})
}
