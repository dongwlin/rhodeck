package control

import (
	"context"
	"errors"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func (r *Run) execute() {
	var client *worker.Client
	var monitorDone chan struct{}
	outcome, code := Stopped, "cancelled_before_start"
	initialized, deviceIssued := false, false
	defer func() {
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
