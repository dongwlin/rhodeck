package maa

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// 注入只经过 fake native 的 callback token；观察结果全部从公开方法读取。
func deliverObservationEvent(t *testing.T, rig *createRig, id MessageID, raw string) {
	t.Helper()
	deliver := native.LookupCallbackDelivery(rig.creator.exToken(0))
	if deliver == nil {
		t.Fatal("instance callback token not registered")
	}
	deliver(int32(id), callbackCBytes(raw))
}

func waitObservationSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangGuard):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertObservationOpen(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s closed early", what)
	default:
	}
}

func TestInstanceObservationTaskCompletionAndNormalClose(t *testing.T) {
	inst, rig := newSyncRig(t, 0x0B5100)
	if inst.Events() == nil || inst.Done() == nil || inst.Fault() == nil {
		t.Fatal("observation channels must be available after successful creation")
	}
	if inst.Events() != inst.Events() || inst.Done() != inst.Done() || inst.Fault() != inst.Fault() {
		t.Fatal("repeated calls must return the same observation channel")
	}
	if err := inst.Err(); err != nil {
		t.Fatalf("ready Err = %v, want nil", err)
	}
	diag := inst.Diagnostics()
	if diag.LostEvents != 0 || diag.FirstFault != nil || diag.StopDiagnostic != nil || len(diag.RawPrefix) != 0 {
		t.Fatalf("ready Diagnostics = %+v, want empty diagnostics", diag)
	}
	assertObservationOpen(t, inst.Done(), "Done")
	assertObservationOpen(t, inst.Fault(), "Fault")

	const raw = `{"what":"AllTasksCompleted","future":9007199254740993}`
	deliverObservationEvent(t, rig, 3, raw)
	ev := mustReadEvent(t, inst.Events())
	if ev.MessageID != 3 || ev.Sequence != 1 || string(ev.JSON) != raw {
		t.Fatalf("task completion event = %+v, want the raw message and sequence 1", ev)
	}
	assertObservationOpen(t, inst.Done(), "Done after task completion")
	assertObservationOpen(t, inst.Fault(), "Fault after normal task completion")

	// 缓冲事件保留到关闭之后；Done 的观察者无需先排空 Events。
	const buffered = `{"what":"FutureMessage"}`
	deliverObservationEvent(t, rig, 99, buffered)
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done")
	if err := inst.Err(); err != nil {
		t.Fatalf("Err after Done = %v, want nil", err)
	}
	ev = mustReadEvent(t, inst.Events())
	if ev.MessageID != 99 || ev.Sequence != 2 || string(ev.JSON) != buffered {
		t.Fatalf("buffered event after close = %+v, want FutureMessage and sequence 2", ev)
	}
	select {
	case _, ok := <-inst.Events():
		if ok {
			t.Fatal("unexpected extra event after Done")
		}
	default:
		t.Fatal("Done is closed but Events is not yet closed")
	}
	if err := waitErr(t, closed, "Instance.Close"); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	assertObservationOpen(t, inst.Fault(), "Fault after normal close")
}

func TestInstanceObservationOverflowPreservesCompletedOperation(t *testing.T) {
	inst, rig := newAsyncRig(t, 0x0B5200, func(l *Limits) { l.PublicEvents = 1 })
	completed, err := inst.Screencap()
	if err != nil {
		t.Fatal(err)
	}
	rig.caller.asyncRets = map[string]int32{"asyncClick": 2}
	pending, err := inst.Click(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	deliverAsyncInfo(t, rig.creator.exToken(0), completed.ID(), "Screencap", `{"ret":true}`)
	before, err := waitOperationGuarded(t, completed, context.Background())
	if err != nil || !before.OK {
		t.Fatalf("first completion = %+v, %v, want success", before, err)
	}
	// 唯一公开缓冲由完成消息占据，消费者停读，后两条均应明确计失。
	deliverObservationEvent(t, rig, 90, `{"future":1}`)
	deliverObservationEvent(t, rig, 91, `{"future":2}`)
	waitObservationSignal(t, inst.Fault(), "overflow Fault")
	assertObservationOpen(t, inst.Done(), "Done with fault only")
	if !errors.Is(inst.Err(), ErrEventOverflow) {
		t.Fatalf("Err during the fault = %v, want ErrEventOverflow", inst.Err())
	}
	if err := inst.Start(); !errors.Is(err, ErrEventOverflow) {
		t.Fatalf("new request after the fault = %v, want ErrEventOverflow", err)
	}
	if _, err := waitOperationGuarded(t, pending, context.Background()); !errors.Is(err, ErrEventOverflow) {
		t.Fatalf("unsettled Operation = %v, want ErrEventOverflow", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done with Events reads stopped")
	final := inst.Err()
	if !errors.Is(final, ErrEventOverflow) {
		t.Fatalf("final Err = %v, want ErrEventOverflow", final)
	}
	diag := inst.Diagnostics()
	if diag.LostEvents != 2 || diag.FirstFault != final || diag.StopDiagnostic != nil {
		t.Fatalf("final Diagnostics = %+v, want 2 lost events with the first cause preserved", diag)
	}
	if err := waitErr(t, closed, "Instance.Close"); err != final {
		t.Fatalf("Close = %v, want the same final state as Err %v", err, final)
	}
	after, err := waitOperationGuarded(t, completed, context.Background())
	if err != nil || after.ID != before.ID || after.OK != before.OK || !bytes.Equal(after.Details, before.Details) {
		t.Fatalf("a subsequent overflow rewrote the completion result: before=%+v after=%+v err=%v", before, after, err)
	}
	_ = mustReadEvent(t, inst.Events())
	if _, ok := <-inst.Events(); ok {
		t.Fatal("Events should be closed after the buffer is drained")
	}
	if later := inst.Diagnostics(); later.LostEvents != diag.LostEvents || later.FirstFault != diag.FirstFault || inst.Err() != final {
		t.Fatalf("diagnostics/error changed after Done: %+v", later)
	}
}

func TestInstanceObservationStopDiagnosticBeforeDestroyReturns(t *testing.T) {
	for _, faultFirst := range []bool{false, true} {
		name := "stop rejection only"
		if faultFirst {
			name = "first cause plus stop rejection"
		}
		t.Run(name, func(t *testing.T) {
			inst, rig := newSyncRig(t, 0x0B5300)
			var first error
			if faultFirst {
				deliverObservationEvent(t, rig, 92, `invalid-json`)
				waitObservationSignal(t, inst.Fault(), "protocol Fault")
				first = inst.Err()
				if !errors.Is(first, ErrProtocol) {
					t.Fatalf("first cause = %v, want ErrProtocol", first)
				}
			}
			rig.closer.stopRet = 0
			destroying := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			rig.closer.onDestroy = func(uintptr) {
				close(destroying)
				<-release
			}
			const closers = 8
			results := make(chan error, closers)
			for range closers {
				go func() { results <- inst.Close() }()
			}
			waitObservationSignal(t, destroying, "entering Destroy")
			assertObservationOpen(t, inst.Done(), "Done before Destroy returns")
			diag := inst.Diagnostics()
			if !errors.Is(diag.StopDiagnostic, ErrNativeRejected) || diag.FirstFault != first {
				t.Fatalf("Diagnostics before Destroy returns = %+v, want the saved stop diagnostic and first cause", diag)
			}
			if !errors.Is(inst.Err(), ErrNativeRejected) || (first != nil && !errors.Is(inst.Err(), first)) {
				t.Fatalf("Err before Destroy returns = %v, want the stop rejection and first cause recognizable", inst.Err())
			}
			if !faultFirst {
				assertObservationOpen(t, inst.Fault(), "Fault with stop rejection only")
			}
			releaseOnce.Do(func() { close(release) })
			waitObservationSignal(t, inst.Done(), "Done")
			final := inst.Err()
			for range closers {
				if err := waitErr(t, results, "concurrent Instance.Close"); err != final {
					t.Fatalf("concurrent Close = %v, want the same final error %v", err, final)
				}
			}
			if err := inst.Close(); err != final || inst.Err() != final {
				t.Fatalf("repeated close changed the final error: Close=%v Err=%v", err, inst.Err())
			}
			later := inst.Diagnostics()
			if later.FirstFault != first || later.StopDiagnostic != diag.StopDiagnostic {
				t.Fatalf("diagnostics changed after Done: before=%+v after=%+v", diag, later)
			}
			if !faultFirst {
				assertObservationOpen(t, inst.Fault(), "Fault after close with stop rejection only")
			}
			assertSingleStopDestroy(t, rig.closer, 0x0B5300)
		})
	}
}

func TestInstanceObservationDiagnosticPrefixOwnership(t *testing.T) {
	inst, rig := newSyncRig(t, 0x0B5400)
	raw := strings.Repeat("invalid-json-", 600)
	deliverObservationEvent(t, rig, 93, raw)
	waitObservationSignal(t, inst.Fault(), "protocol Fault")
	first := inst.Err()
	diag := inst.Diagnostics()
	if !errors.Is(first, ErrProtocol) || diag.FirstFault != first {
		t.Fatalf("protocol first cause not exposed: Err=%v Diagnostics=%+v", first, diag)
	}
	if len(diag.RawPrefix) != 4096 || string(diag.RawPrefix) != raw[:4096] {
		t.Fatalf("first-cause prefix length = %d, want 4096 with the original text preserved", len(diag.RawPrefix))
	}
	ev := mustReadEvent(t, inst.Events())
	if string(ev.JSON) != raw {
		t.Fatal("the invalid JSON event should preserve the full original text for diagnostics")
	}
	diag.RawPrefix[0] = '!'
	ev.JSON[1] = '!'
	deliverObservationEvent(t, rig, 94, `another-invalid-json`)
	_ = mustReadEvent(t, inst.Events())
	if later := inst.Diagnostics(); later.FirstFault != first || string(later.RawPrefix) != raw[:4096] {
		t.Fatal("mutating the public copy or a later fault rewrote the internal first-cause prefix")
	}
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done")
	if err := waitErr(t, closed, "Instance.Close"); err != first || inst.Err() != first {
		t.Fatalf("close changed the first cause: Close=%v Err=%v", err, inst.Err())
	}
	if later := inst.Diagnostics(); later.LostEvents != 0 || string(later.RawPrefix) != raw[:4096] {
		t.Fatalf("first-cause prefix/lost count abnormal after close: %+v", later)
	}
}
