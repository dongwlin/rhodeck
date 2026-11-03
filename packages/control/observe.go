package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

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
	if settled {
		if err := r.persist(); err != nil {
			r.fail("record_write_failed")
			return
		}
	}
	r.notify()
}

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
		if !known && parseErr == nil && id != nil && *id == 0 {
			var classifiers []UnassociatedObservationClassifier
			for _, candidate := range r.reducers {
				if classifier, ok := candidate.(UnassociatedObservationClassifier); ok {
					classifiers = append(classifiers, classifier)
				}
			}
			if len(classifiers) > 0 {
				r.mu.Unlock()
				progress := false
				for _, classifier := range classifiers {
					if classifier.IsUnassociatedProgress(Observation{MessageID: callback.MessageID, CoreSequence: callback.CoreSequence, JSON: bytes.Clone(callback.JSON)}) {
						progress = true
						break
					}
				}
				r.mu.Lock()
				if r.fatal != "" || r.proof != "" || !r.result.FinishedAt.IsZero() {
					r.mu.Unlock()
					return false
				}
				if progress {
					r.mu.Unlock()
					return true
				}
			}
		}
		relevant := false
		if !known && callback.MessageID >= 20000 && callback.MessageID <= 20004 {
			if parseErr != nil {
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
