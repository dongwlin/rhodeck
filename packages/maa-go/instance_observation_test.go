package maa

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
	"github.com/stretchr/testify/require"
)

// 注入只经过 fake native 的 callback token；观察结果全部从公开方法读取。
func deliverObservationEvent(t *testing.T, rig *createRig, id MessageID, raw string) {
	t.Helper()
	deliver := native.LookupCallbackDelivery(rig.creator.exToken(0))
	require.NotNil(t, deliver, "instance callback token not registered")
	deliver(int32(id), callbackCBytes(raw))
}

func waitObservationSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangGuard):
		require.Fail(t, "timed out waiting for "+what)
	}
}

func assertObservationOpen(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		require.Fail(t, what+" closed early")
	default:
	}
}

func TestInstanceObservationTaskCompletionAndNormalClose(t *testing.T) {
	inst, rig := newSyncRig(t, 0x0B5100)
	require.NotNil(t, inst.Events(), "observation channels must be available after successful creation")
	require.NotNil(t, inst.Done(), "observation channels must be available after successful creation")
	require.NotNil(t, inst.Fault(), "observation channels must be available after successful creation")
	require.True(t, inst.Events() == inst.Events() && inst.Done() == inst.Done() && inst.Fault() == inst.Fault(),
		"repeated calls must return the same observation channel")
	readyErr := inst.Err()
	require.NoError(t, readyErr, "ready Err = %v, want nil", readyErr)
	diag := inst.Diagnostics()
	require.Equal(t, uint64(0), diag.LostEvents, "ready Diagnostics = %+v, want empty diagnostics", diag)
	require.Nil(t, diag.FirstFault, "ready Diagnostics = %+v, want empty diagnostics", diag)
	require.Nil(t, diag.StopDiagnostic, "ready Diagnostics = %+v, want empty diagnostics", diag)
	require.Empty(t, diag.RawPrefix, "ready Diagnostics = %+v, want empty diagnostics", diag)
	assertObservationOpen(t, inst.Done(), "Done")
	assertObservationOpen(t, inst.Fault(), "Fault")

	const raw = `{"what":"AllTasksCompleted","future":9007199254740993}`
	deliverObservationEvent(t, rig, 3, raw)
	ev := mustReadEvent(t, inst.Events())
	require.Equal(t, MessageID(3), ev.MessageID, "task completion event = %+v, want the raw message and sequence 1", ev)
	require.Equal(t, uint64(1), ev.Sequence, "task completion event = %+v, want the raw message and sequence 1", ev)
	require.Equal(t, raw, string(ev.JSON), "task completion event = %+v, want the raw message and sequence 1", ev)
	assertObservationOpen(t, inst.Done(), "Done after task completion")
	assertObservationOpen(t, inst.Fault(), "Fault after normal task completion")

	// 缓冲事件保留到关闭之后；Done 的观察者无需先排空 Events。
	const buffered = `{"what":"FutureMessage"}`
	deliverObservationEvent(t, rig, 99, buffered)
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done")
	errAfterDone := inst.Err()
	require.NoError(t, errAfterDone, "Err after Done = %v, want nil", errAfterDone)
	ev = mustReadEvent(t, inst.Events())
	require.Equal(t, MessageID(99), ev.MessageID, "buffered event after close = %+v, want FutureMessage and sequence 2", ev)
	require.Equal(t, uint64(2), ev.Sequence, "buffered event after close = %+v, want FutureMessage and sequence 2", ev)
	require.Equal(t, buffered, string(ev.JSON), "buffered event after close = %+v, want FutureMessage and sequence 2", ev)
	select {
	case _, ok := <-inst.Events():
		require.False(t, ok, "unexpected extra event after Done")
	default:
		require.Fail(t, "Done is closed but Events is not yet closed")
	}
	closeRet := waitErr(t, closed, "Instance.Close")
	require.NoError(t, closeRet, "Close = %v, want nil", closeRet)
	assertObservationOpen(t, inst.Fault(), "Fault after normal close")
}

func TestInstanceObservationOverflowPreservesCompletedOperation(t *testing.T) {
	inst, rig := newAsyncRig(t, 0x0B5200, func(l *Limits) { l.PublicEvents = 1 })
	completed, err := inst.Screencap()
	require.NoError(t, err)
	rig.caller.asyncRets = map[string]int32{"asyncClick": 2}
	pending, err := inst.Click(1, 2)
	require.NoError(t, err)
	deliverAsyncInfo(t, rig.creator.exToken(0), completed.ID(), "Screencap", `{"ret":true}`)
	before, err := waitOperationGuarded(t, completed, context.Background())
	require.NoError(t, err, "first completion = %+v, %v, want success", before, err)
	require.True(t, before.OK, "first completion = %+v, %v, want success", before, err)
	// 唯一公开缓冲由完成消息占据，消费者停读，后两条均应明确计失。
	deliverObservationEvent(t, rig, 90, `{"future":1}`)
	deliverObservationEvent(t, rig, 91, `{"future":2}`)
	waitObservationSignal(t, inst.Fault(), "overflow Fault")
	assertObservationOpen(t, inst.Done(), "Done with fault only")
	require.ErrorIs(t, inst.Err(), ErrEventOverflow, "Err during the fault = %v, want ErrEventOverflow", inst.Err())
	errStart := inst.Start()
	require.ErrorIs(t, errStart, ErrEventOverflow, "new request after the fault = %v, want ErrEventOverflow", errStart)
	_, unsettledErr := waitOperationGuarded(t, pending, context.Background())
	require.ErrorIs(t, unsettledErr, ErrEventOverflow, "unsettled Operation = %v, want ErrEventOverflow", unsettledErr)
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done with Events reads stopped")
	final := inst.Err()
	require.ErrorIs(t, final, ErrEventOverflow, "final Err = %v, want ErrEventOverflow", final)
	diag := inst.Diagnostics()
	require.Equal(t, uint64(2), diag.LostEvents, "final Diagnostics = %+v, want 2 lost events with the first cause preserved", diag)
	require.True(t, diag.FirstFault == final, "final Diagnostics = %+v, want 2 lost events with the first cause preserved", diag)
	require.Nil(t, diag.StopDiagnostic, "final Diagnostics = %+v, want 2 lost events with the first cause preserved", diag)
	closeRet := waitErr(t, closed, "Instance.Close")
	require.True(t, closeRet == final, "Close = %v, want the same final state as Err %v", closeRet, final)
	after, err := waitOperationGuarded(t, completed, context.Background())
	require.NoError(t, err, "a subsequent overflow rewrote the completion result: before=%+v after=%+v err=%v", before, after, err)
	require.Equal(t, before.ID, after.ID, "a subsequent overflow rewrote the completion result: before=%+v after=%+v err=%v", before, after, err)
	require.Equal(t, before.OK, after.OK, "a subsequent overflow rewrote the completion result: before=%+v after=%+v err=%v", before, after, err)
	require.True(t, bytes.Equal(after.Details, before.Details), "a subsequent overflow rewrote the completion result: before=%+v after=%+v err=%v", before, after, err)
	_ = mustReadEvent(t, inst.Events())
	_, eventsOK := <-inst.Events()
	require.False(t, eventsOK, "Events should be closed after the buffer is drained")
	later := inst.Diagnostics()
	require.True(t, later.LostEvents == diag.LostEvents && later.FirstFault == diag.FirstFault && inst.Err() == final,
		"diagnostics/error changed after Done: %+v", later)
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
				require.ErrorIs(t, first, ErrProtocol, "first cause = %v, want ErrProtocol", first)
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
			require.ErrorIs(t, diag.StopDiagnostic, ErrNativeRejected, "Diagnostics before Destroy returns = %+v, want the saved stop diagnostic and first cause", diag)
			require.True(t, diag.FirstFault == first, "Diagnostics before Destroy returns = %+v, want the saved stop diagnostic and first cause", diag)
			require.ErrorIs(t, inst.Err(), ErrNativeRejected, "Err before Destroy returns = %v, want the stop rejection", inst.Err())
			if first != nil {
				require.ErrorIs(t, inst.Err(), first, "Err before Destroy returns = %v, want the first cause recognizable", inst.Err())
			}
			if !faultFirst {
				assertObservationOpen(t, inst.Fault(), "Fault with stop rejection only")
			}
			releaseOnce.Do(func() { close(release) })
			waitObservationSignal(t, inst.Done(), "Done")
			final := inst.Err()
			for range closers {
				closeRet := waitErr(t, results, "concurrent Instance.Close")
				require.True(t, closeRet == final, "concurrent Close = %v, want the same final error %v", closeRet, final)
			}
			err := inst.Close()
			require.True(t, err == final && inst.Err() == final, "repeated close changed the final error: Close=%v Err=%v", err, inst.Err())
			later := inst.Diagnostics()
			require.True(t, later.FirstFault == first && later.StopDiagnostic == diag.StopDiagnostic,
				"diagnostics changed after Done: before=%+v after=%+v", diag, later)
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
	require.ErrorIs(t, first, ErrProtocol, "protocol first cause not exposed: Err=%v Diagnostics=%+v", first, diag)
	require.True(t, diag.FirstFault == first, "protocol first cause not exposed: Err=%v Diagnostics=%+v", first, diag)
	require.Len(t, diag.RawPrefix, 4096, "first-cause prefix length = %d, want 4096 with the original text preserved", len(diag.RawPrefix))
	require.Equal(t, raw[:4096], string(diag.RawPrefix), "first-cause prefix length = %d, want 4096 with the original text preserved", len(diag.RawPrefix))
	ev := mustReadEvent(t, inst.Events())
	require.Equal(t, raw, string(ev.JSON), "the invalid JSON event should preserve the full original text for diagnostics")
	diag.RawPrefix[0] = '!'
	ev.JSON[1] = '!'
	deliverObservationEvent(t, rig, 94, `another-invalid-json`)
	_ = mustReadEvent(t, inst.Events())
	later := inst.Diagnostics()
	require.True(t, later.FirstFault == first && string(later.RawPrefix) == raw[:4096],
		"mutating the public copy or a later fault rewrote the internal first-cause prefix")
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done")
	closeRet := waitErr(t, closed, "Instance.Close")
	require.True(t, closeRet == first && inst.Err() == first, "close changed the first cause: Close=%v Err=%v", closeRet, inst.Err())
	laterAfterClose := inst.Diagnostics()
	require.Equal(t, uint64(0), laterAfterClose.LostEvents, "first-cause prefix/lost count abnormal after close: %+v", laterAfterClose)
	require.Equal(t, raw[:4096], string(laterAfterClose.RawPrefix), "first-cause prefix/lost count abnormal after close: %+v", laterAfterClose)
}
