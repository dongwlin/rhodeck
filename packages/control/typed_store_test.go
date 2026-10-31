package control

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func typedStoreRecord(t *testing.T) Record {
	t.Helper()
	p := validTestPlan(t)
	p.SchemaVersion = SchemaVersion
	p.Tasks[0].Adapter = &AdapterRef{Name: "fixture", Version: 1}
	typed := &TypedResult{SchemaVersion: SchemaVersion, Adapter: *p.Tasks[0].Adapter,
		Targets: []TargetResult{{ID: "target-1", Outcome: TargetAchieved, Details: json.RawMessage(`{"value":9007199254740993}`)}}}
	typed.State, typed.Code = typedState(typed)
	return Record{SchemaVersion: SchemaVersion, Plan: p, Result: Result{
		SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending",
		Tasks: []TaskResult{{ID: p.Tasks[0].ID, CoreTaskID: 1, State: Started, Typed: typed}},
	}}
}

func TestTypedRecordRecoveryPreservesFactsAndVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := typedStoreRecord(t)
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
	got, err := ReadRecord(dir, rec.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.Result.State != Unknown || got.Result.Code != "host_interrupted" ||
		got.Result.Tasks[0].State != Unknown || got.Result.Tasks[0].Typed.State != Succeeded {
		t.Fatalf("recovery changed confirmed facts: %+v", got.Result)
	}
	got.Result.Tasks[0].Typed.Targets[0].Details[2] = 'X'
	got.Plan.Tasks[0].Adapter.Name = "changed"
	next, err := ReadRecord(dir, rec.Result.RunID)
	if err != nil || !bytes.Equal(next.Result.Tasks[0].Typed.Targets[0].Details, rec.Result.Tasks[0].Typed.Targets[0].Details) ||
		next.Plan.Tasks[0].Adapter.Name != "fixture" {
		t.Fatalf("record read aliases mutable state: %+v, %v", next, err)
	}
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
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			rec := typedStoreRecord(t)
			mutate(&rec)
			if err := s.save(rec); err == nil {
				t.Fatal("save accepted incompatible evidence")
			}
			raw, _ := json.Marshal(rec)
			if err := os.WriteFile(filepath.Join(dir, "runs", rec.Result.RunID+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRecord(dir, rec.Result.RunID); err == nil {
				t.Fatal("read accepted incompatible evidence")
			}
		})
	}
}

func TestRecordRejectsDuplicateNestedControlFields(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	rec := typedStoreRecord(t)
	raw, _ := json.Marshal(rec)
	raw = bytes.Replace(raw, []byte(`"outcome":"achieved"`), []byte(`"outcome":"failed","outcome":"achieved"`), 1)
	if err := os.WriteFile(filepath.Join(dir, "runs", rec.Result.RunID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(dir, rec.Result.RunID); err == nil {
		t.Fatal("ambiguous nested control fields accepted")
	}
}

func TestCurrentSchemaRecordsAllowRawAndAdaptedTasks(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	rec := typedStoreRecord(t)
	rec.Plan.Tasks = append(rec.Plan.Tasks, Task{ID: "raw", Type: "FutureTask", Params: json.RawMessage(`{"future":9007199254740993}`)})
	rec.Result.Tasks = append(rec.Result.Tasks, TaskResult{ID: "raw", State: Started})
	if err := s.save(rec); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecord(dir, rec.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.Plan.SchemaVersion != SchemaVersion || got.Result.SchemaVersion != SchemaVersion ||
		got.Result.Tasks[0].Typed.SchemaVersion != SchemaVersion || got.Result.Tasks[1].Typed != nil || got.Plan.Tasks[1].Adapter != nil {
		t.Fatalf("current schema did not retain mixed task forms: %+v", got)
	}
	if !strings.Contains(string(got.Plan.Tasks[1].Params), "9007199254740993") {
		t.Fatal("mixed raw task lost integer precision")
	}
	// Optional adapter and typed fields are naturally omitted for raw tasks.
	rec.Result.RunID = newRunID()
	rec.Plan.Tasks = rec.Plan.Tasks[1:]
	rec.Result.Tasks = rec.Result.Tasks[1:]
	if err := s.save(rec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runs", rec.Result.RunID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"adapter"`)) || bytes.Contains(raw, []byte(`"typed"`)) {
		t.Fatal("raw record emitted optional adapter fields")
	}
	if _, err := ReadRecord(dir, rec.Result.RunID); err != nil {
		t.Fatalf("current record with omitted optional fields was rejected: %v", err)
	}
}
