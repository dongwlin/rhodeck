package control_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/operprogress"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// A typed callback that reaches the monitor before Start was issued cannot be
// associated with an accepted submission. It must not be queued, and it must
// not create a fact; the run reports the stable typed_invalid_event code.
func TestTypedMonitorRejectsCallbackBeforeStart(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	r := newPreparedTypedHarness(t, plan, []int32{101}, false, operprogress.New())
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
	if InspectRunForTest(r).Fatal != "typed_invalid_event" {
		t.Fatalf("early callback fatal = %q, want typed_invalid_event", InspectRunForTest(r).Fatal)
	}
	result := r.Snapshot()
	if result.Tasks[0].Typed.State != Unknown || result.Tasks[0].Typed.Code != "typed_incomplete" {
		t.Fatalf("early callback changed the aggregate: %+v", result.Tasks[0].Typed)
	}
	if target := result.Tasks[0].Typed.Targets[0]; target.Outcome != TargetUnobserved {
		t.Fatalf("early callback created a fact: %+v", target)
	}
	if events := typedEventsIn(r); len(events) != 0 {
		t.Fatalf("early callback published %d typed events", len(events))
	}
}

// A repeated identical fact is a no-op, while a different fact for an already
// confirmed target is a conflict: the runtime stops with typed_conflict and the
// durable record still carries the original fact.
func TestTypedMonitorDuplicateAndConflictingFacts(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	r := newPreparedTypedHarness(t, plan, []int32{101}, true, operprogress.New())
	achieved := `{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo, achieved)
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo, achieved)
	if InspectRunForTest(r).Fatal != "" {
		t.Fatalf("repeating a fact failed the run: %s", InspectRunForTest(r).Fatal)
	}
	if events := typedEventsIn(r); len(events) != 1 {
		t.Fatalf("duplicate fact published %d typed events, want 1", len(events))
	}
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Failed","result_detail":"ResourceInsufficient"}}`)
	if InspectRunForTest(r).Fatal != "typed_conflict" {
		t.Fatalf("conflicting fact fatal = %q, want typed_conflict", InspectRunForTest(r).Fatal)
	}
	result := r.Snapshot()
	target := result.Tasks[0].Typed.Targets[0]
	if target.Outcome != TargetAchieved || target.Code != "completed" || !strings.Contains(string(target.Details), `"reported":2`) {
		t.Fatalf("conflict replaced the confirmed fact: %+v", target)
	}
	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Result.Tasks[0].Typed.Targets[0]; got.Outcome != TargetAchieved || string(got.Details) != string(target.Details) {
		t.Fatalf("durable fact changed: %+v", got)
	}
}

// Retained typed events have their own count and byte bounds inside the total
// event queue. Without a consumer, a real adapter cannot grow history without
// bound: the run reports typed_event_overflow, keeps the facts that were already
// confirmed and persisted, and stops admitting further updates.
func TestTypedMonitorBoundsRetainedEventsWithoutConsumer(t *testing.T) {
	const plans = 40
	entries := make([]string, plans)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"role":"Warrior","name":"Operator-%02d","elite":2}`, i)
	}
	plan := operProgressRuntimePlan(t, `{"plans":[`+strings.Join(entries, ",")+`]}`)
	r := newPreparedTypedHarness(t, plan, []int32{101}, true, operprogress.New())
	coreID := int32(101)
	for i := 0; i < plans; i++ {
		raw := fmt.Sprintf(`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Operator-%02d","result":"Success","result_detail":"Completed","elite":2}}`, i)
		ObserveTypedForTest(r, protocol.Callback{MessageID: operProgressMessageSubTaskExtraInfo, CoreSequence: uint64(i + 1), JSON: json.RawMessage(raw)}, &coreID, nil)
	}
	if InspectRunForTest(r).Fatal != "typed_event_overflow" {
		t.Fatalf("fatal = %q, want typed_event_overflow", InspectRunForTest(r).Fatal)
	}
	if InspectRunForTest(r).TypedEventCount != MaxTypedEventMessages || InspectRunForTest(r).TypedEventBytes > MaxTypedEventRetainedBytes {
		t.Fatalf("retained typed events = %d/%d bytes, want the count bound", InspectRunForTest(r).TypedEventCount, InspectRunForTest(r).TypedEventBytes)
	}
	if len(r.Events()) != MaxTypedEventMessages {
		t.Fatalf("queued typed events = %d, want %d", len(r.Events()), MaxTypedEventMessages)
	}
	// The update that could not publish its event was applied and persisted
	// before publication, so at most one extra confirmed fact may exist.
	achieved := 0
	snapshot := r.Snapshot()
	for _, target := range snapshot.Tasks[0].Typed.Targets {
		if target.Outcome == TargetAchieved {
			achieved++
		}
	}
	if achieved < MaxTypedEventMessages || achieved > MaxTypedEventMessages+1 {
		t.Fatalf("confirmed facts = %d, want %d or %d", achieved, MaxTypedEventMessages, MaxTypedEventMessages+1)
	}
	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	durable := 0
	for _, target := range record.Result.Tasks[0].Typed.Targets {
		if target.Outcome == TargetAchieved {
			durable++
		}
	}
	if durable != achieved {
		t.Fatalf("durable facts = %d, snapshot = %d", durable, achieved)
	}
}
