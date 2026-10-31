package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const (
	runEventMessages = 64
	runEventBytes    = 16 << 20
)

// Runtime owns the data-directory lock and at most one active run. Close is
// repeatable and stops admission immediately; cancellation only stops waiting
// for that shared cleanup, which continues in the background.
type Runtime struct {
	mu        sync.Mutex
	config    Config
	store     *store
	active    *Run
	closed    bool
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func New(cfg Config) (*Runtime, error) {
	if cfg.StopTimeout < 0 || cfg.Worker.StartupTimeout < 0 || cfg.Worker.RequestTimeout < 0 || cfg.Worker.ShutdownTimeout < 0 {
		return nil, errors.New("control: timeouts must not be negative")
	}
	if !filepath.IsAbs(cfg.Worker.Executable) || !utf8.ValidString(cfg.Worker.Executable) || strings.ContainsRune(cfg.Worker.Executable, 0) {
		return nil, errors.New("control: worker executable must be an absolute path")
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	cfg.Worker.Args = append([]string(nil), cfg.Worker.Args...)
	cfg.Worker.Env = append([]string(nil), cfg.Worker.Env...)
	cfg.Adapters = append([]TaskAdapter(nil), cfg.Adapters...)
	s, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Runtime{config: cfg, store: s, closeDone: make(chan struct{})}, nil
}

// Run durably admits an immutable plan before any worker or device action.
// The context controls the run lifetime; cancellation requests graceful stop.
// It is never reused as a command context after a command may have been sent.
func (rt *Runtime) Run(ctx context.Context, plan Plan) (*Run, error) {
	if ctx == nil {
		return nil, errors.New("control: run requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sourcePlan := clonePlan(plan)
	prepared, err := PreparePlan(plan, rt.config.Adapters...)
	if err != nil {
		return nil, err
	}
	plan = prepared.Plan
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return nil, ErrClosed
	}
	if rt.active != nil {
		return nil, ErrBusy
	}
	commands, cancel := context.WithCancel(context.Background())
	r := &Run{
		runtime: rt, plan: plan, sourcePlan: sourcePlan, events: make(chan Event, runEventMessages), done: make(chan struct{}),
		changed: make(chan struct{}, 1), stop: make(chan struct{}), stopSettled: make(chan struct{}), commands: commands, cancelCommands: cancel,
		taskIndex: make(map[int32]int), reducers: prepared.reducers,
		result: Result{SchemaVersion: plan.SchemaVersion, RunID: newRunID(), HostRunID: plan.HostRunID,
			State: Admitted, Cleanup: "pending", StartedAt: time.Now().UTC(), Tasks: prepared.Tasks},
	}
	for i := range r.result.Tasks {
		r.result.Tasks[i].State = Admitted
	}
	if err := rt.store.save(Record{SchemaVersion: sourcePlan.SchemaVersion, Plan: sourcePlan, Result: r.result}); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrRecordWrite, err)
	}
	rt.active = r
	r.emit(Event{Type: "state", State: Admitted})
	go r.execute()
	go func() {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.mu.Lock()
				r.stopReason = "deadline_exceeded"
				r.mu.Unlock()
			}
			r.requestStop()
		case <-r.done:
		}
	}()
	return r, nil
}

func (rt *Runtime) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("control: close requires a context")
	}
	rt.closeOnce.Do(func() {
		rt.mu.Lock()
		rt.closed = true
		r := rt.active
		rt.mu.Unlock()
		go func() {
			if r != nil {
				r.requestStop()
				<-r.done
			}
			err := rt.store.close()
			rt.mu.Lock()
			rt.closeErr = err
			rt.mu.Unlock()
			close(rt.closeDone)
		}()
	})
	select {
	case <-rt.closeDone:
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

func (r *Run) Events() <-chan Event  { return r.events }
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

// Stop shares the same bounded path as lifetime-context cancellation. A
// cancelled wait does not abandon cleanup or report that Core stopped.
func (r *Run) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("control: stop requires a context")
	}
	r.requestStop()
	select {
	case <-r.done:
		s := r.Snapshot()
		if s.State == Unknown || s.Cleanup != "complete" {
			return &protocol.Error{Code: "stop_unconfirmed", Message: "run stop or cleanup could not be confirmed"}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Run) requestStop() {
	select {
	case <-r.done:
		return
	default:
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		r.stopDeadline = time.Now().Add(r.runtime.config.StopTimeout)
		r.mu.Unlock()
		close(r.stop)
		go func() {
			timer := time.NewTimer(r.runtime.config.StopTimeout)
			defer timer.Stop()
			select {
			case <-r.done:
			case <-r.stopSettled:
			case <-timer.C:
				r.fail("stop_timeout")
			}
		}()
	})
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

func (r *Run) emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitLocked(e)
}

func (r *Run) emitLocked(e Event) {
	// Account for entries consumed since the previous send. Only sends and
	// this size ledger use the mutex; receiving never participates in cleanup.
	for len(r.eventSizes) > len(r.events) {
		r.eventBytes -= r.eventSizes[0]
		if r.eventTyped[0] {
			r.typedEventBytes -= r.eventSizes[0]
			r.typedEventCount--
		}
		r.eventSizes = r.eventSizes[1:]
		r.eventTyped = r.eventTyped[1:]
	}
	r.sequence++
	e.SchemaVersion, e.RunID, e.HostRunID = r.result.SchemaVersion, r.result.RunID, r.result.HostRunID
	e.Sequence, e.Time = r.sequence, time.Now().UTC()
	e.Details = bytes.Clone(e.Details)
	e.Typed = cloneTypedResult(e.Typed)
	b, err := json.Marshal(e)
	// Reserve space for the terminal observation even when a consumer stops.
	full := len(r.events) >= cap(r.events)-1 || len(b)+4096 > runEventBytes-r.eventBytes
	if e.Type == "terminal" {
		full = len(r.events) == cap(r.events) || len(b) > runEventBytes-r.eventBytes
	}
	typedFull := e.Type == "typed" && (len(b) > MaxTypedEventBytes || r.typedEventCount >= MaxTypedEventMessages || len(b) > MaxTypedEventRetainedBytes-r.typedEventBytes)
	if err != nil || full || typedFull {
		if r.fatal == "" {
			r.fatal = "event_overflow"
			if typedFull {
				r.fatal = "typed_event_overflow"
			}
		}
		r.cancelCommands()
		r.notify()
		return
	}
	r.events <- e
	r.eventSizes = append(r.eventSizes, len(b))
	r.eventTyped = append(r.eventTyped, e.Type == "typed")
	r.eventBytes += len(b)
	if e.Type == "typed" {
		r.typedEventBytes += len(b)
		r.typedEventCount++
	}
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

func (r *Run) interrupted() bool {
	select {
	case <-r.stop:
		return true
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fatal != ""
}

func (r *Run) execute() {
	var client *worker.Client
	var monitorDone chan struct{}
	outcome, code := Stopped, "cancelled_before_start"
	initialized, deviceIssued := false, false
	defer func() {
		// Stop and shutdown have independent budgets. Once the driver leaves
		// execution, the supervisor's ShutdownTimeout owns process cleanup.
		close(r.stopSettled)
		cleanup, cleanupCode := "complete", ""
		if client != nil {
			err := client.Close(context.Background())
			<-monitorDone
			if err != nil {
				cleanup, cleanupCode = "failed", errorCode(err)
			}
		}
		r.cancelCommands()
		r.mu.Lock()
		if r.proof != "" {
			outcome, code = r.proof, r.proofCode
		}
		if r.fatal != "" {
			if r.proof == "" {
				outcome, code = Unknown, r.fatal
			} else {
				cleanup, cleanupCode = "failed", r.fatal
			}
		}
		// Cleanup is reported separately. A late cleanup failure changes the
		// cleanup fields and the CLI exit code, never an established business
		// state such as succeeded, failed, stopped or rejected.
		r.result.State, r.result.Code = outcome, code
		r.result.Cleanup, r.result.CleanupCode = cleanup, cleanupCode
		r.result.FinishedAt = time.Now().UTC()
		for i := range r.result.Tasks {
			t := &r.result.Tasks[i]
			if t.State != Succeeded && t.State != Failed && t.State != Stopped {
				t.State = outcome
				if outcome == Failed {
					t.State = Unknown
				}
			}
		}
		r.mu.Unlock()
		if err := r.persist(); err != nil {
			r.mu.Lock()
			r.result.Cleanup, r.result.CleanupCode = "failed", "record_write_failed"
			if r.proof == "" {
				r.result.State, r.result.Code = Unknown, "record_write_failed"
			}
			r.mu.Unlock()
		}
		r.mu.Lock()
		r.emitLocked(Event{Type: "terminal", State: r.result.State, Code: r.result.Code})
		// A terminal event may itself encounter an already full consumer queue.
		// The finalized snapshot has already accounted for all prior loss.
		close(r.events)
		r.mu.Unlock()
		r.runtime.mu.Lock()
		r.runtime.active = nil
		close(r.done)
		r.runtime.mu.Unlock()
	}()
	if r.interrupted() {
		return
	}
	var err error
	client, err = worker.Launch(r.commands, r.runtime.config.Worker)
	if err != nil {
		outcome, code = classifyError(err)
		return
	}
	monitorDone = make(chan struct{})
	go func() {
		defer close(monitorDone)
		for m := range client.Events() {
			r.observe(m)
		}
		r.notify()
	}()
	if r.interrupted() {
		return
	}
	info, err := client.Initialize(r.commands, r.plan.Initialize)
	if err != nil {
		outcome, code = classifyError(err)
		return
	}
	initialized = true
	r.mu.Lock()
	r.result.RuntimeInfo = &info
	r.mu.Unlock()
	if err := r.persist(); err != nil {
		outcome, code = Rejected, "record_write_failed"
		return
	}
	if !r.interrupted() {
		r.state(Connecting)
		deviceIssued = true
		op, connectErr := client.Connect(r.commands, r.plan.Connection)
		if connectErr != nil {
			outcome, code = classifyError(connectErr)
			return
		}
		waiting, cancel := context.WithCancel(r.commands)
		waitDone := make(chan struct{})
		go func() {
			select {
			case <-r.stop:
				cancel()
			case <-waitDone:
			}
		}()
		_, err = op.Wait(waiting)
		close(waitDone)
		cancel()
		if err != nil && !r.interrupted() {
			outcome, code = classifyError(err)
			return
		}
	}
	for i, task := range r.plan.Tasks {
		if r.interrupted() {
			break
		}
		accepted, err := client.AppendTask(r.commands, protocol.TaskRequest{Type: task.Type, Params: task.Params})
		if err != nil {
			outcome, code = classifyError(err)
			return
		}
		r.mu.Lock()
		_, duplicate := r.taskIndex[accepted.TaskID]
		if accepted.TaskID > 0 && !duplicate {
			r.taskIndex[accepted.TaskID] = i
			r.result.Tasks[i].CoreTaskID, r.result.Tasks[i].State = accepted.TaskID, Submitted
			r.emitLocked(Event{Type: "task", State: Submitted, TaskID: task.ID, CoreTaskID: accepted.TaskID})
		}
		r.mu.Unlock()
		if accepted.TaskID <= 0 || duplicate {
			r.fail("invalid_task_id")
			break
		}
	}
	if !r.interrupted() {
		r.state(Submitted)
		if err := r.persist(); err != nil {
			outcome, code = Rejected, "record_write_failed"
			return
		}
		r.mu.Lock()
		r.startIssued = true
		r.mu.Unlock()
		if err := client.Start(r.commands); err != nil {
			outcome, code = classifyError(err)
			r.mu.Lock()
			contradiction := r.proof != ""
			r.mu.Unlock()
			if contradiction {
				r.fail("conflicting_start_result")
			}
			return
		}
		r.state(Started)
		for {
			r.mu.Lock()
			complete := r.proof != "" || r.fatal != "" || r.stopObserved
			r.mu.Unlock()
			if complete || r.interrupted() {
				break
			}
			select {
			case <-r.changed:
			case <-r.stop:
			case <-client.Done():
				<-monitorDone
				r.fail(errorCode(client.Err()))
			}
		}
	}
	r.mu.Lock()
	proof, fatal, stopped := r.proof, r.fatal, r.stopObserved
	r.mu.Unlock()
	if proof != "" || fatal != "" {
		return
	}
	if initialized && deviceIssued {
		r.state(Stopping)
		if err := r.stopWorker(client, stopped); err != nil {
			outcome, code = Unknown, errorCode(err)
			return
		}
	}
	outcome, code = Stopped, "stop_confirmed"
	r.mu.Lock()
	if r.stopReason != "" {
		code = r.stopReason
	}
	for _, task := range r.result.Tasks {
		if task.State == Failed {
			outcome, code = Failed, "task_failed"
			break
		}
		if task.Typed != nil && task.Typed.State == Failed {
			outcome, code = Failed, "typed_failed"
			break
		}
	}
	r.mu.Unlock()
}

func (r *Run) stopWorker(client *worker.Client, observed bool) error {
	r.mu.Lock()
	deadline := r.stopDeadline
	r.mu.Unlock()
	if deadline.IsZero() {
		deadline = time.Now().Add(r.runtime.config.StopTimeout)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	stopped, err := client.Stop(ctx)
	if err != nil {
		return err
	}
	if stopped.Stopped || observed {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		observed, fatal := r.stopObserved, r.fatal
		r.mu.Unlock()
		if observed {
			return nil
		}
		if fatal != "" {
			return &protocol.Error{Code: fatal, Message: "stop could not be confirmed"}
		}
		select {
		case <-ctx.Done():
			return &protocol.Error{Code: "stop_timeout", Message: "stop was not confirmed before its deadline"}
		case <-client.Done():
			return &protocol.Error{Code: "worker_exited", Message: "worker exited before stop confirmation"}
		case <-r.changed:
		case <-ticker.C:
			status, err := client.Status(ctx)
			if err != nil {
				return err
			}
			if !status.Running {
				return nil
			}
		}
	}
}

func errorCode(err error) string {
	if err == nil {
		return "worker_exited"
	}
	var wire *protocol.Error
	if errors.As(err, &wire) {
		return wire.Code
	}
	var failure *worker.Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "environment_error"
}

func classifyError(err error) (State, string) {
	var failure *worker.Failure
	if errors.As(err, &failure) && failure.Unknown {
		return Unknown, failure.Code
	}
	return Rejected, errorCode(err)
}

// coreFields reads only the correlation fields; unknown native content remains
// in the raw callback. Ambiguous duplicate fields never settle task outcomes.
func coreFields(raw json.RawMessage) (taskID *int32, finished []int32, err error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, nil, errors.New("control: callback must be an object")
	}
	seen := make(map[string]bool)
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return nil, nil, e
		}
		key := t.(string)
		if seen[key] {
			return nil, nil, errors.New("control: duplicate callback field")
		}
		seen[key] = true
		var value json.RawMessage
		if e := d.Decode(&value); e != nil {
			return nil, nil, e
		}
		switch key {
		case "taskid":
			var id int32
			if bytes.Equal(value, []byte("null")) {
				return nil, nil, errors.New("control: null task ID")
			}
			if e := json.Unmarshal(value, &id); e != nil {
				return nil, nil, e
			}
			taskID = &id
		case "finished_tasks":
			if e := json.Unmarshal(value, &finished); e != nil {
				return nil, nil, e
			}
		}
	}
	return taskID, finished, nil
}

func (r *Run) observe(m protocol.Message) {
	switch m.Name {
	case protocol.FaultEvent:
		var n protocol.FaultNotice
		if err := protocol.Decode(m.Payload, &n); err != nil {
			r.fail("invalid_fault_event")
			return
		}
		r.fail(n.Error.Code)
		return
	case protocol.DiagnosticsEvent:
		var d protocol.InstanceDiagnostics
		if protocol.Decode(m.Payload, &d) != nil {
			r.fail("invalid_diagnostics_event")
		} else if d.LostEvents > 0 {
			r.fail("event_overflow")
		} else if d.StopDiagnostic != nil {
			r.fail(d.StopDiagnostic.Code)
		}
		return
	case protocol.CoreEvent:
	default:
		return
	}
	var callback protocol.Callback
	if err := protocol.Decode(m.Payload, &callback); err != nil {
		r.fail("invalid_core_event")
		return
	}
	id, finished, parseErr := coreFields(callback.JSON)
	e := Event{Type: "core", MessageID: callback.MessageID, CoreSequence: callback.CoreSequence}
	if r.runtime.config.IncludeRawEvents {
		e.Details = bytes.Clone(callback.JSON)
	}
	r.mu.Lock()
	if !r.result.FinishedAt.IsZero() || (len(r.reducers) > 0 && r.proof != "") {
		r.mu.Unlock()
		return
	}
	if id != nil {
		e.CoreTaskID = *id
		if index, ok := r.taskIndex[*id]; ok {
			e.TaskID = r.result.Tasks[index].ID
		}
	}
	r.emitLocked(e)
	r.mu.Unlock()
	if !r.observeTyped(callback, id, parseErr) {
		return
	}
	r.mu.Lock()
	terminalEvent := callback.MessageID == 3 || callback.MessageID == 10000 || callback.MessageID == 10002 || callback.MessageID == 10004
	if parseErr != nil && terminalEvent {
		r.mu.Unlock()
		r.fail("invalid_task_event")
		return
	}
	if callback.MessageID == 0 || callback.MessageID == 1 {
		r.mu.Unlock()
		r.fail("core_error")
		return
	}
	if callback.MessageID == 3 {
		valid := r.startIssued && len(r.taskIndex) == len(r.plan.Tasks) && len(finished) == len(r.plan.Tasks)
		if id != nil {
			_, known := r.taskIndex[*id]
			valid = valid && known
		}
		seen := make(map[int32]bool)
		failed := false
		typedFailed, typedIncomplete := false, false
		for _, taskID := range finished {
			i, ok := r.taskIndex[taskID]
			if !ok || seen[taskID] {
				valid = false
				continue
			}
			seen[taskID] = true
			s := r.result.Tasks[i].State
			if s != Succeeded && s != Failed {
				valid = false
			}
			failed = failed || s == Failed
			if typed := r.result.Tasks[i].Typed; typed != nil {
				typedFailed = typedFailed || typed.State == Failed
				typedIncomplete = typedIncomplete || typed.State != Succeeded
			}
		}
		if valid && r.fatal == "" {
			r.proof = Succeeded
			if failed {
				r.proof = Failed
			}
			if typedFailed {
				r.proof, r.proofCode = Failed, "typed_failed"
			} else if !failed && typedIncomplete {
				r.proof, r.proofCode = Unknown, "typed_incomplete"
			}
		}
		r.mu.Unlock()
		if !valid {
			r.fail("incomplete_task_events")
		} else {
			r.notify()
		}
		return
	}
	if callback.MessageID < 10000 || callback.MessageID > 10004 {
		r.mu.Unlock()
		return
	}
	i, known := 0, false
	if id != nil {
		i, known = r.taskIndex[*id]
	}
	if !known {
		r.mu.Unlock()
		if terminalEvent {
			r.fail("invalid_task_event")
		}
		return
	}
	t := &r.result.Tasks[i]
	previous := t.State
	conflicting := (callback.MessageID == 10000 && (t.State == Succeeded || t.State == Stopped)) ||
		(callback.MessageID == 10002 && (t.State == Failed || t.State == Stopped))
	if conflicting {
		r.mu.Unlock()
		r.fail("conflicting_task_event")
		return
	}
	switch callback.MessageID {
	case 10001:
		if t.State == Submitted {
			t.State = Started
		}
	case 10000:
		if t.State != Succeeded {
			t.State = Failed
		}
	case 10002:
		if t.State != Failed && t.State != Stopped {
			t.State = Succeeded
		}
	case 10004:
		r.stopObserved = true
		if t.State != Succeeded && t.State != Failed {
			t.State = Stopped
		}
	}
	settled := terminal(t.State) && t.State != previous
	r.mu.Unlock()
	// Only terminal task facts require a durable update. The driver persists
	// admission/configuration before Start, and waits for this sole monitor
	// before its final write, so a later write cannot erase a settled task.
	if settled {
		if err := r.persist(); err != nil {
			r.fail("record_write_failed")
			return
		}
	}
	r.notify()
}

// observeTyped runs on the sole worker-event monitor. Reducers receive detached
// input outside the result lock; only validated, bounded, immutable facts enter
// the snapshot, and every change is persisted before it is published.
func (r *Run) observeTyped(callback protocol.Callback, id *int32, parseErr error) bool {
	r.mu.Lock()
	if len(r.reducers) == 0 {
		r.mu.Unlock()
		return true
	}
	if r.fatal != "" || r.proof != "" || !r.result.FinishedAt.IsZero() {
		r.mu.Unlock()
		return false
	}
	i, known := 0, false
	if id != nil {
		i, known = r.taskIndex[*id]
	}
	reducer := r.reducers[i]
	if !known || reducer == nil {
		// Callbacks belonging to a known raw task stay on the raw path. For
		// unassociated subtask events, a matching typed chain cannot be guessed.
		relevant := false
		if !known && callback.MessageID >= 20000 && callback.MessageID <= 20004 {
			if parseErr != nil {
				// An ambiguous correlation object cannot prove that the event
				// belongs to an unrelated chain. Do not select a duplicate's
				// last value through a permissive second decode.
				relevant = true
			} else {
				var fields struct {
					TaskChain string `json:"taskchain"`
				}
				_ = json.Unmarshal(callback.JSON, &fields)
				for index := range r.reducers {
					if fields.TaskChain == "" || fields.TaskChain == r.plan.Tasks[index].Type {
						relevant = true
						break
					}
				}
			}
		}
		r.mu.Unlock()
		if relevant {
			r.fail("typed_invalid_event")
			return false
		}
		return true
	}
	if terminal(r.result.Tasks[i].State) {
		r.mu.Unlock()
		return true
	}
	if parseErr != nil || !r.startIssued {
		r.mu.Unlock()
		r.fail("typed_invalid_event")
		return false
	}
	r.mu.Unlock()
	update, err := reducer.Observe(Observation{MessageID: callback.MessageID, CoreSequence: callback.CoreSequence, JSON: bytes.Clone(callback.JSON)})
	if err != nil {
		r.fail("typed_invalid_event")
		return false
	}
	r.mu.Lock()
	if r.fatal != "" || r.proof != "" || !r.result.FinishedAt.IsZero() || terminal(r.result.Tasks[i].State) {
		r.mu.Unlock()
		return false
	}
	candidate, changed, err := applyTypedUpdate(r.result.Tasks[i].Typed, update)
	if err == nil && changed {
		tasks := append([]TaskResult(nil), r.result.Tasks...)
		tasks[i].Typed = candidate
		err = validateTypedResults(tasks)
	}
	if err != nil {
		r.mu.Unlock()
		code := "typed_invalid_event"
		if errors.Is(err, ErrTypedBudget) {
			code = "typed_result_overflow"
		} else if errors.Is(err, ErrTypedConflict) {
			code = "typed_conflict"
		}
		r.fail(code)
		return false
	}
	if !changed {
		r.mu.Unlock()
		return true
	}
	r.result.Tasks[i].Typed = candidate
	event := Event{Type: "typed", TaskID: r.result.Tasks[i].ID, CoreTaskID: r.result.Tasks[i].CoreTaskID,
		MessageID: callback.MessageID, CoreSequence: callback.CoreSequence, Typed: cloneTypedResult(candidate)}
	r.mu.Unlock()
	if err := r.persist(); err != nil {
		r.fail("record_write_failed")
		return false
	}
	r.emit(event)
	r.notify()
	return true
}
