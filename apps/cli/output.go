package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var errOutputTimeout = errors.New("CLI output deadline exceeded")

type outputJob struct {
	value  any
	result chan error
}

// runOutput keeps one writer and at most one queued object. An arbitrary
// io.Writer cannot be interrupted, so abort leaves at most the current Write
// behind until the process exits; it never prevents worker or lock cleanup.
type runOutput struct {
	jobs  chan outputJob
	abort chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newRunOutput(encoder *json.Encoder) *runOutput {
	w := &runOutput{jobs: make(chan outputJob, 1), abort: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.abort:
				return
			case job, ok := <-w.jobs:
				if !ok {
					return
				}
				select {
				case <-w.abort:
					return
				default:
				}
				job.result <- encoder.Encode(job.value)
			}
		}
	}()
	return w
}

func (w *runOutput) close() {
	w.once.Do(func() {
		close(w.abort)
		close(w.jobs)
	})
}

// write observes run cancellation and completion while output is in flight,
// releasing application ownership even if the output consumer stops reading.
func (w *runOutput) write(ctx context.Context, done <-chan struct{}, value any, grace time.Duration, cleanup func()) error {
	result := make(chan error, 1)
	w.jobs <- outputJob{value: value, result: result}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	cancelled := ctx.Done()
	for {
		select {
		case err := <-result:
			return err
		case <-cancelled:
			cancelled = nil
			cleanup()
		case <-done:
			done = nil
			cleanup()
		case <-timer.C:
			// A completed write wins a simultaneous deadline observation.
			select {
			case err := <-result:
				return err
			default:
				return errOutputTimeout
			}
		}
	}
}
