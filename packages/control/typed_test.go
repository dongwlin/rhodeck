package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type testTaskAdapter struct {
	ref     AdapterRef
	prepare func(Task) (PreparedTask, error)
}

func (a *testTaskAdapter) Descriptor() AdapterRef { return a.ref }
func (a *testTaskAdapter) Prepare(task Task) (PreparedTask, error) {
	if a.prepare != nil {
		return a.prepare(task)
	}
	return PreparedTask{Params: task.Params, Targets: []string{"target"}, Reducer: testTaskReducer{}}, nil
}

type testTaskReducer struct{}

func (testTaskReducer) Observe(Observation) (TypedUpdate, error) { return TypedUpdate{}, nil }

func adaptedTestPlan(t *testing.T) (Plan, *testTaskAdapter) {
	t.Helper()
	p := validTestPlan(t)
	p.SchemaVersion = SchemaVersion
	ref := AdapterRef{Name: "test-adapter", Version: 1}
	p.Tasks[0].Adapter = &ref
	return p, &testTaskAdapter{ref: ref}
}

func testTypedResult(ids ...string) *TypedResult {
	r := &TypedResult{SchemaVersion: SchemaVersion, Adapter: AdapterRef{Name: "test", Version: 1}}
	for _, id := range ids {
		r.Targets = append(r.Targets, TargetResult{ID: id, Outcome: TargetUnobserved})
	}
	r.State, r.Code = typedState(r)
	return r
}

func TestPreparePlanExplicitAdapterAndOwnership(t *testing.T) {
	p, adapter := adaptedTestPlan(t)
	p.Tasks = append(p.Tasks, Task{ID: "raw", Type: p.Tasks[0].Type, Params: json.RawMessage(`{"future":9007199254740993,"future":2}`)})
	calls := 0
	var returnedParams json.RawMessage
	adapter.prepare = func(task Task) (PreparedTask, error) {
		calls++
		returnedParams = bytes.Clone(task.Params)
		// Mutating this detached input cannot change either the caller's plan
		// or the immutable adapter selection in the prepared plan.
		task.Params[2] = 'X'
		task.Adapter.Name = "mutated"
		return PreparedTask{Params: returnedParams, Targets: []string{"target"}, Reducer: testTaskReducer{}}, nil
	}
	prepared, err := PreparePlan(p, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || prepared.Tasks[1].Typed != nil || len(prepared.reducers) != 1 {
		t.Fatalf("adapter was selected without an explicit reference: calls=%d, tasks=%+v", calls, prepared.Tasks)
	}
	if !bytes.Equal(prepared.Plan.Tasks[0].Params, p.Tasks[0].Params) || !bytes.Equal(prepared.Plan.Tasks[1].Params, p.Tasks[1].Params) {
		t.Fatal("Core params lost unknown fields or integer literals")
	}
	if prepared.Plan.Tasks[0].Adapter.Name != adapter.ref.Name || p.Tasks[0].Adapter.Name != adapter.ref.Name {
		t.Fatal("adapter preparation mutated a plan reference")
	}
	expectedParams := bytes.Clone(prepared.Plan.Tasks[0].Params)
	returnedParams[2] = 'Y'
	p.Tasks[0].Params[2] = 'Z'
	p.Tasks[0].Adapter.Name = "caller-mutated"
	prepared.Plan.Initialize.ResourceRoots[0] = "preview-mutated"
	if !bytes.Equal(prepared.Plan.Tasks[0].Params, expectedParams) || !bytes.Contains(prepared.Plan.Tasks[0].Params, []byte("9007199254740993")) {
		t.Fatal("prepared params alias adapter output or caller input")
	}
	if prepared.Plan.Tasks[0].Adapter.Name != adapter.ref.Name || p.Initialize.ResourceRoots[0] == "preview-mutated" {
		t.Fatal("prepared plan aliases caller data")
	}
	if got := prepared.Tasks[0].Typed; got.State != Unknown || got.Targets[0].Outcome != TargetUnobserved || got.Code != "typed_incomplete" {
		t.Fatalf("admission invented a business fact: %+v", got)
	}
}

func TestPreparePlanRawTypeDoesNotSelectAdapter(t *testing.T) {
	p := validTestPlan(t)
	p.Tasks[0].Type = "OperProgress"
	p.Tasks[0].Params = json.RawMessage(`{"future":9007199254740993,"future":2}`)
	called := false
	adapter := &testTaskAdapter{ref: AdapterRef{Name: "OperProgress", Version: 1}, prepare: func(Task) (PreparedTask, error) {
		called = true
		return PreparedTask{}, errors.New("must not execute")
	}}
	got, err := PreparePlan(p, adapter)
	if err != nil || called || got.Tasks[0].Typed != nil || !bytes.Equal(got.Plan.Tasks[0].Params, p.Tasks[0].Params) {
		t.Fatalf("generic input changed: called=%t, error=%v, task=%+v", called, err, got.Tasks)
	}
}

func TestPreparePlanRejectsInvalidSelectionsAndCompilerOutput(t *testing.T) {
	for name, mutate := range map[string]func(*Plan, *testTaskAdapter) []TaskAdapter{
		"missing registry": func(*Plan, *testTaskAdapter) []TaskAdapter { return nil },
		"unknown version": func(p *Plan, a *testTaskAdapter) []TaskAdapter {
			p.Tasks[0].Adapter.Version++
			return []TaskAdapter{a}
		},
		"duplicate registry": func(_ *Plan, a *testTaskAdapter) []TaskAdapter { return []TaskAdapter{a, a} },
		"nil adapter":        func(*Plan, *testTaskAdapter) []TaskAdapter { return []TaskAdapter{nil} },
		"typed nil adapter":  func(*Plan, *testTaskAdapter) []TaskAdapter { return []TaskAdapter{(*testTaskAdapter)(nil)} },
		"future schema": func(p *Plan, a *testTaskAdapter) []TaskAdapter {
			p.SchemaVersion = SchemaVersion + 1
			return []TaskAdapter{a}
		},
		"zero version": func(p *Plan, a *testTaskAdapter) []TaskAdapter {
			p.Tasks[0].Adapter.Version = 0
			return []TaskAdapter{a}
		},
		"nil reducer":       invalidPrepared(func(p *PreparedTask) { p.Reducer = nil }),
		"nil params":        invalidPrepared(func(p *PreparedTask) { p.Params = nil }),
		"array params":      invalidPrepared(func(p *PreparedTask) { p.Params = json.RawMessage(`[]`) }),
		"zero targets":      invalidPrepared(func(p *PreparedTask) { p.Targets = nil }),
		"duplicate targets": invalidPrepared(func(p *PreparedTask) { p.Targets = []string{"same", "same"} }),
		"empty target":      invalidPrepared(func(p *PreparedTask) { p.Targets = []string{""} }),
		"target count":      invalidPrepared(func(p *PreparedTask) { p.Targets = make([]string, MaxTypedTargetsPerTask+1) }),
		"compiled bytes": invalidPrepared(func(p *PreparedTask) {
			p.Params = json.RawMessage(`{"x":"` + strings.Repeat("x", MaxAdapterParamsBytes) + `"}`)
		}),
	} {
		t.Run(name, func(t *testing.T) {
			p, adapter := adaptedTestPlan(t)
			registry := mutate(&p, adapter)
			if _, err := PreparePlan(p, registry...); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("invalid adapter input accepted: %v", err)
			}
		})
	}
}

func invalidPrepared(mutate func(*PreparedTask)) func(*Plan, *testTaskAdapter) []TaskAdapter {
	return func(_ *Plan, adapter *testTaskAdapter) []TaskAdapter {
		adapter.prepare = func(task Task) (PreparedTask, error) {
			out := PreparedTask{Params: task.Params, Targets: []string{"target"}, Reducer: testTaskReducer{}}
			mutate(&out)
			return out, nil
		}
		return []TaskAdapter{adapter}
	}
}

func TestAdapterControlDecoding(t *testing.T) {
	p, _ := adaptedTestPlan(t)
	raw, _ := json.Marshal(p)
	for _, changed := range []string{
		strings.Replace(string(raw), `"name":"test-adapter"`, `"name":"test-adapter","name":"other"`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1,"future":true`, 1),
		strings.Replace(string(raw), `"schema_version":1`, `"schema_version":2`, 1),
	} {
		if _, err := DecodePlan(strings.NewReader(changed)); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("ambiguous or future adapter plan accepted: %v", err)
		}
	}
	if _, err := DecodePlan(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestTypedFactsAggregateWithoutClaimingAchievement(t *testing.T) {
	for _, outcome := range []TargetOutcome{TargetAchieved, TargetAlreadySet, TargetFailed, TargetLocked, TargetNotFound,
		TargetUnobserved, TargetReportedTrainingStarted, TargetReportedPrerequisiteStarted, TargetConfirmationObserved, TargetAmbiguous} {
		t.Run(string(outcome), func(t *testing.T) {
			got, _, err := applyTypedUpdate(testTypedResult("one"), TypedUpdate{Targets: []TargetResult{{ID: "one", Outcome: outcome}}})
			if err != nil {
				t.Fatal(err)
			}
			want := Unknown
			switch outcome {
			case TargetAchieved, TargetAlreadySet:
				want = Succeeded
			case TargetFailed, TargetLocked, TargetNotFound:
				want = Failed
			}
			if got.State != want {
				t.Fatalf("%s became %s; want %s", outcome, got.State, want)
			}
		})
	}
	r := testTypedResult("one", "two")
	partial, _, err := applyTypedUpdate(r, TypedUpdate{Targets: []TargetResult{{ID: "one", Outcome: TargetAchieved}}})
	if err != nil || partial.State != Unknown {
		t.Fatalf("partial success became complete: %+v, %v", partial, err)
	}
	failed, _, err := applyTypedUpdate(partial, TypedUpdate{Targets: []TargetResult{{ID: "two", Outcome: TargetFailed}}})
	if err != nil || failed.State != Failed || failed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("partial failure lost facts: %+v, %v", failed, err)
	}
}

func TestTypedUpdateImmutableFactsAndDetachedJSON(t *testing.T) {
	original := testTypedResult("one", "two")
	update := TypedUpdate{Targets: []TargetResult{{ID: "one", Outcome: TargetAchieved, Details: json.RawMessage(`{"value":9007199254740993}`)}}, Summary: json.RawMessage(`{"count":1}`)}
	got, changed, err := applyTypedUpdate(original, update)
	if err != nil || !changed {
		t.Fatalf("fact not admitted: %v, %t", err, changed)
	}
	if original.Targets[0].Outcome != TargetUnobserved || len(original.Summary) != 0 {
		t.Fatal("update modified the original")
	}
	if _, changed, err := applyTypedUpdate(got, update); err != nil || changed {
		t.Fatalf("identical fact was not idempotent: %v, %t", err, changed)
	}
	update.Targets[0].Details[2] = 'X'
	update.Summary[2] = 'X'
	if !bytes.Contains(got.Targets[0].Details, []byte(`"value":9007199254740993`)) || string(got.Summary) != `{"count":1}` {
		t.Fatal("retained typed result aliases adapter JSON")
	}
	copy := cloneTypedResult(got)
	copy.Targets[0].Details[2] = 'Y'
	copy.Summary[2] = 'Y'
	copy.Targets[0].Outcome = TargetFailed
	if got.Targets[0].Outcome != TargetAchieved || !bytes.Contains(got.Targets[0].Details, []byte(`"value"`)) || string(got.Summary) != `{"count":1}` {
		t.Fatal("clone aliases retained typed facts")
	}
	for _, update := range []TypedUpdate{
		{Targets: []TargetResult{{ID: "one", Outcome: TargetFailed}}},
		{Summary: json.RawMessage(`{"count":2}`)},
	} {
		if _, _, err := applyTypedUpdate(got, update); !errors.Is(err, ErrTypedConflict) {
			t.Fatalf("conflicting fact accepted: %v", err)
		}
		if got.Targets[0].Outcome != TargetAchieved || string(got.Summary) != `{"count":1}` {
			t.Fatal("rejected update destroyed confirmed evidence")
		}
	}
}

func TestTypedUpdateRejectsMalformedOrFutureFactsAtomically(t *testing.T) {
	for name, update := range map[string]TypedUpdate{
		"unknown target":   {Targets: []TargetResult{{ID: "other", Outcome: TargetAchieved}}},
		"duplicate target": {Targets: []TargetResult{{ID: "one", Outcome: TargetAchieved}, {ID: "one", Outcome: TargetFailed}}},
		"future outcome":   {Targets: []TargetResult{{ID: "one", Outcome: "future"}}},
		"null detail":      {Targets: []TargetResult{{ID: "one", Outcome: TargetAchieved, Details: json.RawMessage(`null`)}}},
		"array summary":    {Summary: json.RawMessage(`[]`)},
		"nested duplicate": {Summary: json.RawMessage(`{"params":{"json":[{"x":1,"x":2}]}}`)},
		"unobserved fact":  {Targets: []TargetResult{{ID: "one", Outcome: TargetUnobserved, Code: "reported"}}},
	} {
		t.Run(name, func(t *testing.T) {
			original := testTypedResult("one")
			if _, _, err := applyTypedUpdate(original, update); !errors.Is(err, ErrTypedResult) {
				t.Fatalf("bad fact accepted: %v", err)
			}
			if original.Targets[0].Outcome != TargetUnobserved || len(original.Summary) != 0 {
				t.Fatal("rejected update changed the original")
			}
		})
	}
	future := testTypedResult("one")
	future.SchemaVersion++
	if err := validateTypedResult(future); !errors.Is(err, ErrTypedResult) {
		t.Fatalf("future result schema accepted: %v", err)
	}
	wrong := testTypedResult("one")
	wrong.State, wrong.Code = Succeeded, ""
	if err := validateTypedResult(wrong); !errors.Is(err, ErrTypedResult) {
		t.Fatalf("fabricated success accepted: %v", err)
	}
}

func sizedObject(size int) json.RawMessage {
	return json.RawMessage(`{"x":"` + strings.Repeat("x", size-8) + `"}`)
}

func TestTypedDetailAndSummaryByteBoundaries(t *testing.T) {
	for _, summary := range []bool{false, true} {
		limit := MaxTypedDetailBytes
		if summary {
			limit = MaxTypedSummaryBytes
		}
		for _, delta := range []int{0, 1} {
			update := TypedUpdate{}
			if summary {
				update.Summary = sizedObject(limit + delta)
			} else {
				update.Targets = []TargetResult{{ID: "one", Outcome: TargetAchieved, Details: sizedObject(limit + delta)}}
			}
			_, _, err := applyTypedUpdate(testTypedResult("one"), update)
			if delta == 0 && err != nil || delta == 1 && !errors.Is(err, ErrTypedBudget) {
				t.Fatalf("summary=%t delta=%d: %v", summary, delta, err)
			}
		}
	}
}

func TestTypedCountAndAggregateByteBudgets(t *testing.T) {
	ids := make([]string, MaxTypedTargetsPerTask)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	within := testTypedResult(ids...)
	if err := validateTypedResult(within); err != nil {
		t.Fatal(err)
	}
	outside := cloneTypedResult(within)
	outside.Targets = append(outside.Targets, TargetResult{ID: "overflow", Outcome: TargetUnobserved})
	if err := validateTypedResult(outside); !errors.Is(err, ErrTypedBudget) {
		t.Fatalf("target count bound not enforced: %v", err)
	}
	four := make([]TaskResult, 4)
	for i := range four {
		four[i].Typed = cloneTypedResult(within)
	}
	if err := validateTypedResults(four); err != nil {
		t.Fatalf("exact run target budget rejected: %v", err)
	}
	if err := validateTypedResults(append(four, TaskResult{Typed: testTypedResult("overflow")})); !errors.Is(err, ErrTypedBudget) {
		t.Fatalf("run target count bound not enforced: %v", err)
	}
	// Keep below the count limit while independently crossing retained bytes.
	var tasks []TaskResult
	for i := 0; i < 9; i++ {
		r := testTypedResult()
		for j := 0; j < 15; j++ {
			r.Targets = append(r.Targets, TargetResult{ID: fmt.Sprint(j), Outcome: TargetAchieved, Details: sizedObject(MaxTypedDetailBytes)})
		}
		r.State, r.Code = typedState(r)
		if err := validateTypedResult(r); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, TaskResult{Typed: r})
	}
	if err := validateTypedResults(tasks[:8]); err != nil {
		t.Fatalf("bounded aggregate rejected: %v", err)
	}
	if err := validateTypedResults(tasks); !errors.Is(err, ErrTypedBudget) {
		t.Fatalf("run byte budget not enforced: %v", err)
	}
	oversize := cloneTypedResult(tasks[0].Typed)
	oversize.Targets = append(oversize.Targets, TargetResult{ID: "extra", Outcome: TargetAchieved, Details: sizedObject(MaxTypedDetailBytes)})
	if err := validateTypedResult(oversize); !errors.Is(err, ErrTypedBudget) {
		t.Fatalf("task byte budget not enforced: %v", err)
	}
}

func TestPreparePlanAdapterInputByteBoundary(t *testing.T) {
	for _, delta := range []int{0, 1} {
		p, adapter := adaptedTestPlan(t)
		p.Tasks[0].Params = sizedObject(MaxAdapterParamsBytes + delta)
		_, err := PreparePlan(p, adapter)
		if delta == 0 && err != nil || delta == 1 && !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("adapter input byte boundary delta=%d: %v", delta, err)
		}
	}
}
