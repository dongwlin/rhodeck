package control

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

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
