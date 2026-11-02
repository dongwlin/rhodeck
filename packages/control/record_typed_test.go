// Covers: record.go, store.go and objective.go.

package control

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func typedStoreRecord(t *testing.T) Record {
	t.Helper()
	p := validTestPlan(t)
	p.SchemaVersion = SchemaVersion
	p.Tasks[0].Adapter = &AdapterRef{Name: "fixture", Version: 1}
	typed := &TypedResult{SchemaVersion: SchemaVersion, Adapter: *p.Tasks[0].Adapter, Policy: ObjectiveAll,
		Targets: []TargetResult{{ID: "target-1", Outcome: TargetAchieved, Details: json.RawMessage(`{"value":9007199254740993}`)}}}
	typed.State, typed.Code = typedState(typed)
	return Record{SchemaVersion: SchemaVersion, Plan: p, Result: Result{
		SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending",
		Tasks: []TaskResult{{ID: p.Tasks[0].ID, CoreTaskID: 1, State: Started, Policy: ObjectiveAll, Typed: typed}},
	}}
}

func TestTypedRecordRecoveryPreservesFactsAndVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	require.NoError(t, err)
	rec := typedStoreRecord(t)
	require.NoError(t, s.save(rec))
	require.NoError(t, s.close())
	s, err = openStore(dir)
	require.NoError(t, err)
	defer s.close()
	got, err := ReadRecord(dir, rec.Result.RunID)
	require.NoError(t, err)
	require.True(t, got.SchemaVersion == SchemaVersion && got.Result.State == Unknown && got.Result.Code == "host_interrupted" &&
		got.Result.Tasks[0].State == Unknown && got.Result.Tasks[0].Typed.State == Succeeded,
		"recovery changed confirmed facts: %+v", got.Result)
	got.Result.Tasks[0].Typed.Targets[0].Details[2] = 'X'
	got.Plan.Tasks[0].Adapter.Name = "changed"
	next, err := ReadRecord(dir, rec.Result.RunID)
	require.True(t, err == nil && bytes.Equal(next.Result.Tasks[0].Typed.Targets[0].Details, rec.Result.Tasks[0].Typed.Targets[0].Details) &&
		next.Plan.Tasks[0].Adapter.Name == "fixture",
		"record read aliases mutable state: %+v, %v", next, err)
}

func TestRecordRejectsIncompatibleTypedData(t *testing.T) {
	for name, mutate := range map[string]func(*Record){
		"future record":             func(r *Record) { r.SchemaVersion = SchemaVersion + 1 },
		"future nested":             func(r *Record) { r.Result.Tasks[0].Typed.SchemaVersion++ },
		"result version":            func(r *Record) { r.Result.SchemaVersion = SchemaVersion + 1 },
		"plan version":              func(r *Record) { r.Plan.SchemaVersion = SchemaVersion + 1 },
		"adapter mismatch":          func(r *Record) { r.Result.Tasks[0].Typed.Adapter.Version++ },
		"task mismatch":             func(r *Record) { r.Result.Tasks[0].ID = "other" },
		"missing typed":             func(r *Record) { r.Result.Tasks[0].Typed = nil },
		"missing task":              func(r *Record) { r.Result.Tasks = nil },
		"false success":             func(r *Record) { r.Result.Tasks[0].Typed.Targets[0].Outcome = TargetUnobserved },
		"unconfirmed chain success": func(r *Record) { r.Result.State = Succeeded },
		"incomplete typed success": func(r *Record) {
			r.Result.State, r.Result.Tasks[0].State = Succeeded, Succeeded
			typed := r.Result.Tasks[0].Typed
			typed.Targets[0] = TargetResult{ID: "target-1", Outcome: TargetUnobserved}
			typed.State, typed.Code = typedState(typed)
		},
		"failed typed success": func(r *Record) {
			r.Result.State, r.Result.Tasks[0].State = Succeeded, Succeeded
			typed := r.Result.Tasks[0].Typed
			typed.Targets[0].Outcome = TargetFailed
			typed.State, typed.Code = typedState(typed)
		},
		"oversized detail": func(r *Record) {
			r.Result.Tasks[0].Typed.Targets[0].Details = json.RawMessage(`{"x":"` + strings.Repeat("x", MaxTypedDetailBytes) + `"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := openStore(dir)
			require.NoError(t, err)
			defer s.close()
			rec := typedStoreRecord(t)
			mutate(&rec)
			require.Error(t, s.save(rec), "save accepted incompatible evidence")
			raw, _ := json.Marshal(rec)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "runs", rec.Result.RunID+".json"), raw, 0600))
			_, err = ReadRecord(dir, rec.Result.RunID)
			require.Error(t, err, "read accepted incompatible evidence")
		})
	}
}

func TestCurrentSchemaRecordsAllowRawAndAdaptedTasks(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	require.NoError(t, err)
	defer s.close()
	rec := typedStoreRecord(t)
	rec.Plan.Tasks = append(rec.Plan.Tasks, Task{ID: "raw", Type: "FutureTask", Params: json.RawMessage(`{"future":9007199254740993}`)})
	rec.Result.Tasks = append(rec.Result.Tasks, TaskResult{ID: "raw", State: Started})
	require.NoError(t, s.save(rec))
	got, err := ReadRecord(dir, rec.Result.RunID)
	require.NoError(t, err)
	require.True(t, got.SchemaVersion == SchemaVersion && got.Plan.SchemaVersion == SchemaVersion && got.Result.SchemaVersion == SchemaVersion &&
		got.Result.Tasks[0].Typed.SchemaVersion == SchemaVersion && got.Result.Tasks[1].Typed == nil && got.Plan.Tasks[1].Adapter == nil,
		"current schema did not retain mixed task forms: %+v", got)
	require.True(t, strings.Contains(string(got.Plan.Tasks[1].Params), "9007199254740993"), "mixed raw task lost integer precision")
	// Optional adapter and typed fields are naturally omitted for raw tasks.
	rec.Result.RunID = newRunID()
	rec.Plan.Tasks = rec.Plan.Tasks[1:]
	rec.Result.Tasks = rec.Result.Tasks[1:]
	require.NoError(t, s.save(rec))
	raw, err := os.ReadFile(filepath.Join(dir, "runs", rec.Result.RunID+".json"))
	require.NoError(t, err)
	require.False(t, bytes.Contains(raw, []byte(`"adapter"`)) || bytes.Contains(raw, []byte(`"typed"`)), "raw record emitted optional adapter fields")
	_, err = ReadRecord(dir, rec.Result.RunID)
	require.NoError(t, err, "current record with omitted optional fields was rejected: %v", err)
}

func policyStoreRecord(t *testing.T, policy ObjectivePolicy) Record {
	t.Helper()
	rec := typedStoreRecord(t)
	task := &rec.Result.Tasks[0]
	task.Policy = policy
	if policy == ObjectiveNone {
		task.Typed = nil
		return rec
	}
	task.Typed.Policy = policy
	task.Typed.Targets = []TargetResult{
		{ID: "round-1", Outcome: TargetFailed, Code: "round_failed"},
		{ID: "round-2", Outcome: TargetAchieved, Details: json.RawMessage(`{"value":9007199254740993}`)},
		{ID: "round-3", Outcome: TargetUnobserved},
	}
	task.Typed.Summary = json.RawMessage(`{"rounds":9007199254740993}`)
	task.Typed.State, task.Typed.Code = typedState(task.Typed)
	return rec
}

func TestObjectivePolicyRecordsRoundTripAndDetach(t *testing.T) {
	for _, policy := range []ObjectivePolicy{ObjectiveAll, ObjectiveAny, ObjectiveNone} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			s, err := openStore(dir)
			require.NoError(t, err)
			defer s.close()
			rec := policyStoreRecord(t, policy)
			require.NoError(t, s.save(rec))
			got, err := ReadRecord(dir, rec.Result.RunID)
			require.True(t, err == nil && reflect.DeepEqual(got, rec), "policy round trip changed evidence: got=%+v, error=%v", got, err)
			raw, err := os.ReadFile(filepath.Join(dir, "runs", rec.Result.RunID+".json"))
			require.NoError(t, err)
			require.True(t, bytes.Contains(raw, []byte(`"objective_policy":"`+string(policy)+`"`)), "record omitted the objective policy: %s", raw)
			if policy == ObjectiveNone {
				require.False(t, bytes.Contains(raw, []byte(`"typed"`)), "input-only record manufactured typed facts: %s", raw)
			}
			got.Result.Tasks[0].Policy = "mutated"
			got.Plan.Tasks[0].Adapter.Name = "mutated"
			if typed := got.Result.Tasks[0].Typed; typed != nil {
				typed.Policy = "mutated"
				typed.Targets[1].Details[2] = 'X'
				typed.Targets[0].Code = "mutated"
				typed.Summary[2] = 'X'
			}
			again, err := ReadRecord(dir, rec.Result.RunID)
			require.True(t, err == nil && reflect.DeepEqual(again, rec), "mutable read changed persisted facts: got=%+v, error=%v", again, err)
		})
	}
}

func TestInputOnlyRecordsPreserveChainTerminalStates(t *testing.T) {
	for _, state := range []State{Succeeded, Failed, Stopped} {
		t.Run(string(state), func(t *testing.T) {
			dir := t.TempDir()
			s, err := openStore(dir)
			require.NoError(t, err)
			defer s.close()
			rec := policyStoreRecord(t, ObjectiveNone)
			rec.Result.State, rec.Result.Tasks[0].State = state, state
			rec.Result.Cleanup = "complete"
			require.NoError(t, s.save(rec))
			got, err := ReadRecord(dir, rec.Result.RunID)
			require.True(t, err == nil && got.Result.State == state && got.Result.Tasks[0].Typed == nil && got.Result.Tasks[0].Policy == ObjectiveNone,
				"input-only terminal record changed facts: got=%+v, error=%v", got, err)
		})
	}
}

func TestObjectivePolicyRecoveryPreservesEvidenceWithoutAdapters(t *testing.T) {
	for _, policy := range []ObjectivePolicy{ObjectiveAny, ObjectiveNone} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			s, err := openStore(dir)
			require.NoError(t, err)
			rec := policyStoreRecord(t, policy)
			require.NoError(t, s.save(rec))
			require.NoError(t, s.close())
			// Opening storage has no adapter registry or worker configuration.
			// Even an already-achieved any objective cannot complete a run
			// whose native chain was interrupted.
			s, err = openStore(dir)
			require.NoError(t, err)
			defer s.close()
			got, err := ReadRecord(dir, rec.Result.RunID)
			require.NoError(t, err)
			require.True(t, got.Result.State == Unknown && got.Result.Code == "host_interrupted" && got.Result.Cleanup == "unconfirmed" && got.Result.Tasks[0].State == Unknown,
				"interrupted chain acquired a conclusion: %+v", got.Result)
			require.True(t, got.Result.Tasks[0].Policy == policy && reflect.DeepEqual(got.Result.Tasks[0].Typed, rec.Result.Tasks[0].Typed) && reflect.DeepEqual(got.Plan, rec.Plan),
				"recovery reinterpreted or lost objective evidence: %+v", got)
			first := cloneResult(got.Result)
			require.NoError(t, s.close())
			s, err = openStore(dir)
			require.NoError(t, err)
			defer s.close()
			again, err := ReadRecord(dir, rec.Result.RunID)
			require.True(t, err == nil && reflect.DeepEqual(again.Result, first), "second recovery rewrote terminal evidence: got=%+v, error=%v", again, err)
		})
	}
}

func TestObjectivePolicyRecordsRejectInvalidCombinations(t *testing.T) {
	for name, mutate := range map[string]func(*Record){
		"missing policy":        func(r *Record) { r.Result.Tasks[0].Policy = "" },
		"unknown policy":        func(r *Record) { r.Result.Tasks[0].Policy = "future" },
		"unknown typed policy":  func(r *Record) { r.Result.Tasks[0].Typed.Policy = "future" },
		"missing typed policy":  func(r *Record) { r.Result.Tasks[0].Typed.Policy = "" },
		"policy mismatch":       func(r *Record) { r.Result.Tasks[0].Policy = ObjectiveAny },
		"none with typed facts": func(r *Record) { r.Result.Tasks[0].Policy = ObjectiveNone },
		"none typed payload": func(r *Record) {
			r.Result.Tasks[0].Policy, r.Result.Tasks[0].Typed.Policy = ObjectiveNone, ObjectiveNone
			r.Result.Tasks[0].Typed.Targets = nil
		},
		"all without typed": func(r *Record) { r.Result.Tasks[0].Typed = nil },
		"any without typed": func(r *Record) {
			r.Result.Tasks[0].Policy, r.Result.Tasks[0].Typed = ObjectiveAny, nil
		},
		"all without targets": func(r *Record) { r.Result.Tasks[0].Typed.Targets = nil },
		"any without targets": func(r *Record) {
			r.Result.Tasks[0].Policy, r.Result.Tasks[0].Typed.Policy = ObjectiveAny, ObjectiveAny
			r.Result.Tasks[0].Typed.Targets = nil
		},
		"raw with typed": func(r *Record) { r.Plan.Tasks[0].Adapter = nil },
		"raw with all policy": func(r *Record) {
			r.Plan.Tasks[0].Adapter, r.Result.Tasks[0].Typed = nil, nil
		},
		"raw with none policy": func(r *Record) {
			r.Plan.Tasks[0].Adapter, r.Result.Tasks[0].Typed = nil, nil
			r.Result.Tasks[0].Policy = ObjectiveNone
		},
		"any incomplete success": func(r *Record) {
			r.Result.State, r.Result.Tasks[0].State = Succeeded, Succeeded
			r.Result.Tasks[0].Policy, r.Result.Tasks[0].Typed.Policy = ObjectiveAny, ObjectiveAny
			r.Result.Tasks[0].Typed.Targets[0] = TargetResult{ID: "target-1", Outcome: TargetUnobserved}
			r.Result.Tasks[0].Typed.State, r.Result.Tasks[0].Typed.Code = typedState(r.Result.Tasks[0].Typed)
		},
		"none unconfirmed chain success": func(r *Record) {
			r.Result.Tasks[0].Policy, r.Result.Tasks[0].Typed = ObjectiveNone, nil
			r.Result.State = Succeeded
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := openStore(dir)
			require.NoError(t, err)
			rec := typedStoreRecord(t)
			mutate(&rec)
			require.Error(t, s.save(rec), "save accepted invalid objective policy data")
			require.NoError(t, s.close())
			raw, err := json.Marshal(rec)
			require.NoError(t, err)
			path := filepath.Join(dir, "runs", rec.Result.RunID+".json")
			require.NoError(t, os.WriteFile(path, raw, 0600))
			_, err = ReadRecord(dir, rec.Result.RunID)
			require.Error(t, err, "read accepted invalid objective policy data")
			recovered, openErr := openStore(dir)
			if openErr == nil {
				_ = recovered.close()
			}
			require.Error(t, openErr, "startup accepted invalid objective policy data")
			after, err := os.ReadFile(path)
			require.True(t, err == nil && bytes.Equal(after, raw), "rejection altered evidence: error=%v", err)
		})
	}
}
