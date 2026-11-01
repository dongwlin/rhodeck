package control

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// Run is a single immutable submission, its bounded observations and result.
// Events has exactly one consumer. Done confirms worker cleanup and persistence;
// buffered events remain readable afterward. Snapshot returns detached copies.
type Run struct {
	mu              sync.Mutex
	persistMu       sync.Mutex
	runtime         *Runtime
	plan            Plan
	sourcePlan      Plan // Preserve explicit input separately from compiled Core params.
	result          Result
	events          chan Event
	done            chan struct{}
	changed         chan struct{}
	stop            chan struct{}
	stopSettled     chan struct{}
	stopOnce        sync.Once
	stopDeadline    time.Time
	stopReason      string
	commands        context.Context
	cancelCommands  context.CancelFunc
	taskIndex       map[int32]int
	reducers        map[int]TaskReducer
	sequence        uint64
	eventSizes      []int
	eventTyped      []bool
	eventBytes      int
	typedEventBytes int
	typedEventCount int
	fatal           string
	proof           State
	proofCode       string
	stopObserved    bool
	startIssued     bool
}

func (r *Run) Events() <-chan Event { return r.events }

func (r *Run) Done() <-chan struct{} { return r.done }

func cloneResult(in Result) Result {
	in.Tasks = append([]TaskResult(nil), in.Tasks...)
	for i := range in.Tasks {
		in.Tasks[i].Typed = cloneTypedResult(in.Tasks[i].Typed)
	}
	if in.RuntimeInfo != nil {
		v := *in.RuntimeInfo
		v.Resources = append([]protocol.Resource(nil), v.Resources...)
		if v.Limits != nil {
			l := *v.Limits
			v.Limits = &l
		}
		in.RuntimeInfo = &v
	}
	return in
}

func (r *Run) Snapshot() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneResult(r.result)
}

func (r *Run) notify() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *Run) fail(code string) {
	r.mu.Lock()
	if !r.result.FinishedAt.IsZero() {
		r.mu.Unlock()
		return
	}
	if r.fatal == "" {
		r.fatal = code
	}
	r.mu.Unlock()
	r.cancelCommands()
	r.notify()
}

func (r *Run) state(state State) {
	r.mu.Lock()
	r.result.State = state
	r.emitLocked(Event{Type: "state", State: state})
	r.mu.Unlock()
}

func (r *Run) persist() error {
	// Serialize the snapshot and save together: an earlier driver snapshot
	// must never replace a newer fact already saved by the callback monitor.
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	r.mu.Lock()
	record := Record{SchemaVersion: r.sourcePlan.SchemaVersion, Plan: r.sourcePlan, Result: cloneResult(r.result)}
	r.mu.Unlock()
	if err := r.runtime.store.save(record); err != nil {
		return fmt.Errorf("%w: %w", ErrRecordWrite, err)
	}
	return nil
}
