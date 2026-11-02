// Covers: observe.go, events.go and adapter.go.

package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

type testObservationClassifier struct {
	classify func(Observation) bool
}

func (*testObservationClassifier) Observe(Observation) (TypedUpdate, error) {
	return TypedUpdate{}, nil
}

func (c *testObservationClassifier) IsUnassociatedProgress(o Observation) bool {
	return c.classify(o)
}

func classifierCallback() protocol.Callback {
	return protocol.Callback{MessageID: 20003, CoreSequence: 7, JSON: []byte(`{"taskid":0,"taskchain":"FutureTask"}`)}
}

func TestRuntimeClassifierCanReadSnapshotAndReceivesDetachedJSON(t *testing.T) {
	var run *Run
	var snapshot Result
	classifier := &testObservationClassifier{classify: func(o Observation) bool {
		snapshot = run.Snapshot()
		o.JSON[0] = '['
		return true
	}}
	run = typedObservationHarness(t, classifier)
	callback := classifierCallback()
	original := bytes.Clone(callback.JSON)
	accepted := make(chan bool, 1)
	go func() {
		id := int32(0)
		accepted <- run.observeTyped(callback, &id, nil)
	}()
	select {
	case ok := <-accepted:
		require.True(t, ok && snapshot.RunID == run.Snapshot().RunID, "classifier failed to inspect its run: accepted=%v snapshot=%+v", ok, snapshot)
	case <-time.After(3 * time.Second):
		require.FailNow(t, "classifier could not read Snapshot while holding the result lock")
	}
	require.True(t, bytes.Equal(callback.JSON, original), "classifier changed the source callback: %q", callback.JSON)
	require.False(t, run.fatal != "" || len(typedEventsIn(run)) != 0 || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved, "progress classifier created a target fact: fatal=%q result=%+v", run.fatal, run.Snapshot())
}

func TestRuntimeClassifierCannotChangeFallbackAssociation(t *testing.T) {
	run := typedObservationHarness(t, &testObservationClassifier{classify: func(o Observation) bool {
		// Changing the classifier's input cannot hide a callback associated
		// with a typed chain from the monitor's subsequent correlation checks.
		copy(o.JSON, bytes.ReplaceAll(o.JSON, []byte("FutureTask"), []byte("OtherChain")))
		return false
	}})
	callback := classifierCallback()
	id := int32(0)
	require.False(t, run.observeTyped(callback, &id, nil) || run.fatal != "typed_invalid_event", "classifier changed fallback association: fatal=%q callback=%s", run.fatal, callback.JSON)
}

func TestRuntimeClassifierDoesNotBlockControlAndRejectsLateProgress(t *testing.T) {
	for _, ending := range []string{"failure_after_close", "proof", "finished"} {
		t.Run(ending, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseClassifier := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseClassifier)
			run := typedObservationHarness(t, &testObservationClassifier{classify: func(Observation) bool {
				close(entered)
				<-release
				return true
			}})
			accepted := make(chan bool, 1)
			go func() {
				id := int32(0)
				accepted <- run.observeTyped(classifierCallback(), &id, nil)
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				require.FailNow(t, "classifier did not enter")
			}
			controlDone := make(chan error, 1)
			if ending == "failure_after_close" {
				run.stop = make(chan struct{})
				run.stopSettled = make(chan struct{})
				run.runtime.config.StopTimeout = 5 * time.Second
				run.runtime.active = run
				run.runtime.closeDone = make(chan struct{})
				t.Cleanup(func() {
					releaseClassifier()
					close(run.stopSettled)
					close(run.done)
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_ = run.runtime.Close(ctx)
				})
			}
			go func() {
				_ = run.Snapshot()
				if ending == "failure_after_close" {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					if err := run.runtime.Close(ctx); !errors.Is(err, context.Canceled) {
						controlDone <- fmt.Errorf("Runtime.Close returned %v, want context.Canceled", err)
						return
					}
					// Close must reach requestStop even while the classifier is blocked.
					<-run.stop
					if err := run.Stop(ctx); !errors.Is(err, context.Canceled) {
						controlDone <- fmt.Errorf("Run.Stop returned %v, want context.Canceled", err)
						return
					}
					run.fail("test_failure")
				} else {
					run.mu.Lock()
					if ending == "proof" {
						run.proof = Succeeded
					} else {
						run.result.FinishedAt = time.Now().UTC()
					}
					run.mu.Unlock()
				}
				controlDone <- nil
			}()
			select {
			case err := <-controlDone:
				require.NoError(t, err, "control operation failed while classifier was blocked")
			case <-time.After(3 * time.Second):
				require.FailNow(t, "classifier held the result lock against control operations")
			}
			releaseClassifier()
			select {
			case ok := <-accepted:
				require.False(t, ok, "accepted classifier progress after run termination")
			case <-time.After(3 * time.Second):
				require.FailNow(t, "classifier did not return after release")
			}
			require.False(t, len(typedEventsIn(run)) != 0 || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved, "late classifier changed target facts: %+v", run.Snapshot())
		})
	}
}

// A consumer that stops draining must not keep the worker or the data-directory
// lock alive after an event overflow. The run stops unknown and a new runtime
// can take ownership immediately after cleanup.
func TestRuntimeEventOverflowReleasesCleanupAndDirectory(t *testing.T) {
	cfg := runtimeConfig(t, "overflow")
	cfg.IncludeRawEvents = true
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	cfg.Worker.Env = append(cfg.Worker.Env, "RHODECK_TEST_CONTROL_WORKER_PID="+pidFile)
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	require.NoError(t, err)
	select {
	case <-r.Done():
	case <-time.After(60 * time.Second):
		require.FailNow(t, "cleanup depended on the event consumer")
	}
	result := r.Snapshot()
	require.True(t, result.State == Unknown && result.Code == "event_overflow", "overflow result = %+v", result)
	require.NoError(t, rt.Close(context.Background()))
	requireWorkerProcessExited(t, pidFile)
	next, err := New(cfg)
	require.NoError(t, err, "directory lock was retained after event overflow: %v", err)
	require.NoError(t, next.Close(context.Background()))
}
