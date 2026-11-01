package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
