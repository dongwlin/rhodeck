package control

import (
	"context"
	"errors"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

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
