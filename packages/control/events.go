package control

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	runEventMessages = 64
	runEventBytes    = 16 << 20
)

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
