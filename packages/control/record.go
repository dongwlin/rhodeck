package control

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/dongwlin/rhodeck/packages/control/internal/recordfs"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const maxRecordBytes = MaxPlanBytes + (1 << 20)

var runIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func terminal(s State) bool {
	return s == Succeeded || s == Failed || s == Stopped || s == Rejected || s == Unknown
}

// ReadRecord reads a bounded private snapshot without acquiring runtime
// ownership. Replacement is atomic, so readers observe an old or new record.
func ReadRecord(dataDir, runID string) (Record, error) {
	var record Record
	if !validPath(dataDir, 65536) || !runIDPattern.MatchString(runID) {
		return record, errors.New("control: invalid record path")
	}
	raw, err := recordfs.Read(dataDir, runID+".json", maxRecordBytes)
	if err != nil {
		return record, recordFSError(err)
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
		if task.ID != result.ID {
			return errors.New("control: record task association mismatch")
		}
		if task.Adapter == nil {
			if result.Policy != "" || result.Typed != nil {
				return errors.New("control: raw record task contains objective data")
			}
		} else {
			switch result.Policy {
			case ObjectiveNone:
				if result.Typed != nil {
					return errors.New("control: input-only record task contains typed facts")
				}
			case ObjectiveAll, ObjectiveAny:
				if result.Typed == nil || *task.Adapter != result.Typed.Adapter || result.Policy != result.Typed.Policy {
					return errors.New("control: record adapter or objective association mismatch")
				}
			default:
				return errors.New("control: record task has an unsupported objective policy")
			}
		}
		if record.Result.State == Succeeded && (result.State != Succeeded || (result.Typed != nil && result.Typed.State != Succeeded)) {
			return errors.New("control: record success lacks confirmed task facts")
		}
	}
	return validateTypedResults(record.Result.Tasks)
}
