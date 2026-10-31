package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// typedAcceptanceSourcePlan is a realistic current-schema plan: two explicit
// adapter tasks and one open raw task with a large integer literal. The strict
// input order differs from the compiled Core order, so a record can prove it
// kept the caller's original parameters.
func typedAcceptanceSourcePlan(t *testing.T) Plan {
	t.Helper()
	p := validTestPlan(t)
	oper := AdapterRef{Name: operProgressAdapterName, Version: operProgressVersion}
	theme := AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
	p.Tasks = []Task{
		{ID: "grow-1", Type: operProgressTaskType, Adapter: &oper, Params: json.RawMessage(`{"plans":[{"name":"Alpha","role":"Warrior","elite":2}]}`)},
		{ID: "theme-1", Type: switchThemeTaskType, Adapter: &theme, Params: json.RawMessage(`{"themes":["Theme One","Theme Two"]}`)},
		{ID: "raw-1", Type: "FutureTask", Params: json.RawMessage(`{"future":9007199254740993,"unknown":{"a":1}}`)},
	}
	return p
}

// typedAcceptanceRecord builds an in-flight record with confirmed business
// facts for both adapters plus one untouched raw task. It is a valid current
// schema record that recovery may rewrite to the interrupted state.
func typedAcceptanceRecord(t *testing.T) Record {
	t.Helper()
	plan := typedAcceptanceSourcePlan(t)
	prepared, err := PreparePlan(plan, NewOperProgressAdapter(), NewSwitchThemeAdapter())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []int32{101, 202, 303} {
		prepared.Tasks[i].CoreTaskID, prepared.Tasks[i].State = id, Started
	}
	prepared.Tasks[0].Typed.Targets[0] = TargetResult{
		ID: "op-0001", Outcome: TargetAchieved, Code: "completed",
		Details: json.RawMessage(`{"action":"elite","expected":2,"reported":2}`),
	}
	prepared.Tasks[1].Typed.Targets[0] = TargetResult{
		ID: "theme-selection", Outcome: TargetLocked, Code: "locked",
		Details: json.RawMessage(`{"theme":"Theme Two"}`),
	}
	for i := range prepared.Tasks {
		if typed := prepared.Tasks[i].Typed; typed != nil {
			typed.State, typed.Code = typedState(typed)
		}
	}
	return Record{SchemaVersion: SchemaVersion, Plan: clonePlan(plan), Result: Result{
		SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending", Tasks: prepared.Tasks,
	}}
}

func writeRawRecord(t *testing.T, dir string, raw []byte, runID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runs", runID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// Current schema 1 accepts raw, adapter and mixed plans. Optional adapter and
// typed fields may be absent. Every container that claims another version,
// including the trial schema 2, is rejected instead of migrated.
func TestTypedSchemaBoundaryAcceptsCurrentAndRejectsFutureVersions(t *testing.T) {
	plan := typedAcceptanceSourcePlan(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePlan(bytes.NewReader(raw)); err != nil {
		t.Fatalf("current mixed plan rejected: %v", err)
	}
	prepared, err := PreparePlan(plan, NewOperProgressAdapter(), NewSwitchThemeAdapter())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Tasks[2].Typed != nil || prepared.Plan.Tasks[2].Adapter != nil {
		t.Fatal("raw task gained adapter or typed fields")
	}
	if !bytes.Contains(prepared.Plan.Tasks[2].Params, []byte("9007199254740993")) {
		t.Fatalf("raw literal changed during preparation: %s", prepared.Plan.Tasks[2].Params)
	}

	// Optional fields absent is ordinary current schema, not a compatibility
	// mode: a raw-only plan carries no adapter or typed field at all.
	rawPlan := validTestPlan(t)
	rawOnly, err := json.Marshal(rawPlan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawOnly, []byte(`"adapter"`)) || bytes.Contains(rawOnly, []byte(`"typed"`)) {
		t.Fatalf("raw plan emitted optional fields: %s", rawOnly)
	}
	if _, err := DecodePlan(bytes.NewReader(rawOnly)); err != nil {
		t.Fatalf("raw plan without optional fields rejected: %v", err)
	}

	futurePlan := bytes.Replace(raw, []byte(`"schema_version":1`), []byte(`"schema_version":2`), 1)
	if _, err := DecodePlan(bytes.NewReader(futurePlan)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("schema 2 plan error = %v, want ErrInvalidPlan", err)
	}

	for name, mutate := range map[string]func(*Record){
		"record version 2":          func(r *Record) { r.SchemaVersion = 2 },
		"result version 2":          func(r *Record) { r.Result.SchemaVersion = 2 },
		"plan version 2":            func(r *Record) { r.Plan.SchemaVersion = 2 },
		"typed version 2":           func(r *Record) { r.Result.Tasks[0].Typed.SchemaVersion = 2 },
		"adapter version mismatch":  func(r *Record) { r.Result.Tasks[1].Typed.Adapter.Version = 2 },
		"missing typed association": func(r *Record) { r.Result.Tasks[0].Typed = nil },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			mutated := typedAcceptanceRecord(t)
			mutate(&mutated)
			rawRecord, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			writeRawRecord(t, dir, rawRecord, mutated.Result.RunID)
			if _, err := ReadRecord(dir, mutated.Result.RunID); err == nil {
				t.Fatal("read accepted unsupported or inconsistent evidence")
			}
			if s, err := openStore(dir); err == nil {
				_ = s.close()
				t.Fatal("directory startup accepted unsupported or inconsistent evidence")
			}
		})
	}
}

// A corrupted record, or one whose task, adapter and typed associations do not
// match, is rejected by both ReadRecord and directory startup. Nothing is
// migrated, deleted or replayed.
func TestTypedRecordRejectsCorruptionAndBrokenAssociations(t *testing.T) {
	rec := typedAcceptanceRecord(t)
	valid, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"truncated json":  valid[:len(valid)/2],
		"duplicate field": bytes.Replace(valid, []byte(`"outcome":"achieved"`), []byte(`"outcome":"failed","outcome":"achieved"`), 1),
		"null result":     []byte(`{"schema_version":1,"plan":null,"result":null}`),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeRawRecord(t, dir, raw, rec.Result.RunID)
			if _, err := ReadRecord(dir, rec.Result.RunID); err == nil {
				t.Fatal("read accepted a corrupt record")
			}
			if s, err := openStore(dir); err == nil {
				_ = s.close()
				t.Fatal("startup accepted a corrupt record")
			}
		})
	}
}

// Recovery rewrites only unfinished chain and run state. Confirmed target facts,
// original raw parameters and typed aggregates survive, and reading needs no
// registered adapter: nothing is prepared, launched or replayed.
func TestTypedRecordRecoveryRetainsConfirmedFactsWithoutPreparation(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := typedAcceptanceRecord(t)
	if err := s.save(rec); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	// No Config, adapter or worker exists here. Open and read only.
	s, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	got, err := ReadRecord(dir, rec.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result.State != Unknown || got.Result.Code != "host_interrupted" ||
		got.Result.Cleanup != "unconfirmed" || got.Result.CleanupCode != "host_interrupted" {
		t.Fatalf("recovered run state = %+v", got.Result)
	}
	for i, task := range got.Result.Tasks {
		if task.State != Unknown {
			t.Fatalf("task %d state = %s, want unknown", i, task.State)
		}
	}
	grow, theme, rawTask := got.Result.Tasks[0].Typed, got.Result.Tasks[1].Typed, got.Result.Tasks[2].Typed
	if grow == nil || grow.State != Succeeded || grow.Targets[0].Outcome != TargetAchieved ||
		grow.Targets[0].Code != "completed" || !strings.Contains(string(grow.Targets[0].Details), `"reported":2`) {
		t.Fatalf("confirmed operator fact lost: %+v", grow)
	}
	if theme == nil || theme.State != Failed || theme.Code != "typed_failed" ||
		theme.Targets[0].Outcome != TargetLocked || !bytes.Equal(theme.Targets[0].Details, json.RawMessage(`{"theme":"Theme Two"}`)) {
		t.Fatalf("confirmed theme fact lost: %+v", theme)
	}
	if rawTask != nil {
		t.Fatalf("raw task gained typed facts: %+v", rawTask)
	}
	// The private record keeps the caller's original plan, not compiled params.
	if !bytes.Contains(got.Plan.Tasks[0].Params, []byte(`"name":"Alpha","role":"Warrior"`)) ||
		bytes.Contains(got.Plan.Tasks[0].Params, []byte(`"role":"Warrior","name":"Alpha"`)) {
		t.Fatalf("record did not keep the original plan: %s", got.Plan.Tasks[0].Params)
	}
	if !bytes.Contains(got.Plan.Tasks[2].Params, []byte("9007199254740993")) {
		t.Fatalf("raw integer literal lost: %s", got.Plan.Tasks[2].Params)
	}

	// A second startup sees a terminal recovered record and leaves it alone.
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	again, err := ReadRecord(dir, rec.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Result.Code != "host_interrupted" || again.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("second recovery changed a terminal record: %+v", again.Result)
	}
}
