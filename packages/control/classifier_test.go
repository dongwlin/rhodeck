package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
		if !ok || snapshot.RunID != run.Snapshot().RunID {
			t.Fatalf("classifier failed to inspect its run: accepted=%v snapshot=%+v", ok, snapshot)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("classifier could not read Snapshot while holding the result lock")
	}
	if !bytes.Equal(callback.JSON, original) {
		t.Fatalf("classifier changed the source callback: %q", callback.JSON)
	}
	if run.fatal != "" || len(typedEventsIn(run)) != 0 || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved {
		t.Fatalf("progress classifier created a target fact: fatal=%q result=%+v", run.fatal, run.Snapshot())
	}
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
	if run.observeTyped(callback, &id, nil) || run.fatal != "typed_invalid_event" {
		t.Fatalf("classifier changed fallback association: fatal=%q callback=%s", run.fatal, callback.JSON)
	}
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
				t.Fatal("classifier did not enter")
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
				if err != nil {
					t.Fatalf("control operation failed while classifier was blocked: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("classifier held the result lock against control operations")
			}
			releaseClassifier()
			select {
			case ok := <-accepted:
				if ok {
					t.Fatal("accepted classifier progress after run termination")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("classifier did not return after release")
			}
			if len(typedEventsIn(run)) != 0 || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved {
				t.Fatalf("late classifier changed target facts: %+v", run.Snapshot())
			}
		})
	}
}
