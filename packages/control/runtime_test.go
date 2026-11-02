// Covers: run.go, stop.go, execute.go, runtime.go and events.go.

package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		state State
		clean string
	}{
		{"success", Succeeded, "complete"}, {"failure", Failed, "complete"}, {"suberror", Succeeded, "complete"},
		{"errorstopped", Failed, "complete"}, {"completedstopped", Stopped, "complete"},
		{"latestopped", Succeeded, "complete"}, {"initreject", Rejected, "complete"}, {"connectreject", Rejected, "complete"},
		{"appendreject", Rejected, "complete"}, {"startreject", Rejected, "complete"},
		{"missingcompletion", Unknown, "complete"}, {"missingbatchid", Unknown, "complete"},
		{"extrabatchid", Unknown, "complete"}, {"duplicatebatchid", Unknown, "complete"},
		{"foreign", Unknown, "complete"}, {"malformed", Unknown, "complete"}, {"duplicatetaskid", Unknown, "complete"},
		{"crash", Unknown, "failed"}, {"starthang", Unknown, "failed"}, {"closehang", Succeeded, "failed"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg := runtimeConfig(t, tc.mode)
			rt := newRuntime(t, cfg)
			r, err := rt.Run(context.Background(), runtimePlan(t))
			require.NoError(t, err)
			events, result := waitRuntimeRun(t, r)
			require.True(t, result.State == tc.state && result.Cleanup == tc.clean, "result = %+v, want %s/%s", result, tc.state, tc.clean)
			for i, e := range events {
				require.True(t, e.Sequence == uint64(i+1) && e.RunID == result.RunID && e.HostRunID == "host-run" && e.SchemaVersion == SchemaVersion,
					"invalid event correlation: %+v", e)
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			require.True(t, err == nil && record.Result.State == result.State && record.Result.Cleanup == result.Cleanup,
				"record = %+v, %v", record.Result, err)
			if tc.mode == "failure" {
				require.True(t, result.Tasks[0].State == Failed && result.Tasks[1].State == Succeeded, "partial task facts lost: %+v", result.Tasks)
			}
			if tc.mode == "errorstopped" {
				require.True(t, result.Tasks[0].State == Failed && result.Tasks[1].State == Unknown, "error followed by stop lost task facts: %+v", result.Tasks)
			}
			if tc.mode == "completedstopped" {
				require.True(t, result.Tasks[0].State == Succeeded && result.Tasks[1].State == Stopped, "completion followed by stop lost task facts: %+v", result.Tasks)
			}
			if tc.mode == "closehang" {
				require.Equal(t, Succeeded, result.Tasks[0].State, "cleanup rewrote task facts: %+v", result.Tasks)
			}
		})
	}
}

func TestRuntimeStopAndLifetimeCancellation(t *testing.T) {
	for _, mode := range []string{"stop", "stopwait", "stopcallback", "stopstuck"} {
		t.Run(mode, func(t *testing.T) {
			rt := newRuntime(t, runtimeConfig(t, mode))
			ctx, cancel := context.WithCancel(context.Background())
			r, err := rt.Run(ctx, runtimePlan(t))
			require.NoError(t, err)
			waitStarted(t, r)
			cancel()
			_, result := waitRuntimeRun(t, r)
			want := Stopped
			if mode == "stopstuck" {
				want = Unknown
			}
			require.Equal(t, want, result.State, "result = %+v, want %s", result, want)
		})
	}
}

func TestRuntimeSlowConsumerDoesNotBlockCleanup(t *testing.T) {
	for _, mode := range []string{"overflow", "byteoverflow"} {
		t.Run(mode, func(t *testing.T) {
			cfg := runtimeConfig(t, mode)
			cfg.IncludeRawEvents = true
			rt := newRuntime(t, cfg)
			r, err := rt.Run(context.Background(), runtimePlan(t))
			require.NoError(t, err)
			// The byte-overflow fixture delivers six 3 MiB callbacks, so decoding
			// them dominates wall time under the race detector; cleanup itself
			// finishes within milliseconds. This bound only has to distinguish
			// "cleanup finished" from "cleanup waits for the consumer forever".
			select {
			case <-r.Done():
			case <-time.After(60 * time.Second):
				require.FailNow(t, "cleanup depended on event consumer")
			}
			result := r.Snapshot()
			require.True(t, result.State == Unknown && result.Code == "event_overflow", "result = %+v", result)
			count := 0
			for range r.Events() {
				count++
			}
			require.True(t, count <= runEventMessages && count > 0, "retained events = %d", count)
		})
	}
}

func TestRuntimeConcurrentStopCloseAndAdmission(t *testing.T) {
	cfg := runtimeConfig(t, "stop")
	rt := newRuntime(t, cfg)
	plan := runtimePlan(t)
	r, err := rt.Run(context.Background(), plan)
	require.NoError(t, err)
	_, err = rt.Run(context.Background(), plan)
	require.ErrorIs(t, err, ErrBusy, "concurrent run = %v", err)
	_, err = New(cfg)
	require.ErrorIs(t, err, ErrBusy, "concurrent directory owner = %v", err)
	waitStarted(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			assert.NoError(t, r.Stop(context.Background()))
		})
		wg.Go(func() {
			assert.NoError(t, rt.Close(context.Background()))
		})
	}
	wg.Wait()
	result := r.Snapshot()
	require.True(t, result.State == Stopped && result.Cleanup == "complete", "result = %+v", result)
	_, err = rt.Run(context.Background(), plan)
	require.ErrorIs(t, err, ErrClosed, "closed runtime admission = %v", err)
	next, err := New(cfg)
	require.NoError(t, err, "directory was not released: %v", err)
	_ = next.Close(context.Background())
}

func TestRuntimeUnknownCallbackAndSnapshotOwnership(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			cfg := runtimeConfig(t, "unknowncallback")
			cfg.IncludeRawEvents = raw
			rt := newRuntime(t, cfg)
			plan := runtimePlan(t)
			r, err := rt.Run(context.Background(), plan)
			require.NoError(t, err)
			plan.Tasks[0].Params[0] = '['
			plan.Initialize.ResourceRoots[0] = "mutated"
			events, result := waitRuntimeRun(t, r)
			found := false
			for _, e := range events {
				if e.MessageID != 99999 {
					continue
				}
				found = true
				require.True(t, raw == (len(e.Details) != 0) && (!raw || strings.Contains(string(e.Details), "9007199254740993")),
					"raw event policy lost: %+v", e)
			}
			require.True(t, found && result.State == Succeeded, "result = %+v, unknown callback present = %t", result, found)
			result.Tasks[0].State = Failed
			result.RuntimeInfo.Resources[0].Root = "mutated"
			result.RuntimeInfo.Limits.InputJSONBytes = 1
			next := r.Snapshot()
			require.True(t, next.Tasks[0].State == Succeeded && next.RuntimeInfo.Resources[0].Root != "mutated" && next.RuntimeInfo.Limits.InputJSONBytes != 1,
				"mutable snapshot alias: %+v", next)
		})
	}
}

func TestRuntimeCancelledCloseKeepsOwnershipUntilCleanup(t *testing.T) {
	cfg := runtimeConfig(t, "stopwait")
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	require.NoError(t, err)
	waitStarted(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Close(ctx); err != nil {
		require.ErrorIs(t, err, context.Canceled)
	}
	require.NoError(t, rt.Close(context.Background()))
	result := r.Snapshot()
	require.True(t, result.State == Stopped && result.Cleanup == "complete", "background cleanup = %+v", result)
}

func TestRuntimeFinalRecordWriteFailureIsVisible(t *testing.T) {
	cfg := runtimeConfig(t, "finalwritefail")
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	require.NoError(t, err)
	waitStarted(t, r)
	runs := filepath.Join(cfg.DataDir, "runs")
	require.NoError(t, os.RemoveAll(runs))
	require.NoError(t, os.WriteFile(runs, []byte("blocked"), 0600))
	err = r.Stop(context.Background())
	require.Error(t, err, "record failure was reported as confirmed cleanup")
	result := r.Snapshot()
	require.True(t, result.State == Unknown && result.CleanupCode == "record_write_failed", "result = %+v", result)
}

func TestRuntimeStopAndShutdownHaveIndependentBudgets(t *testing.T) {
	cfg := runtimeConfig(t, "slowclose")
	cfg.StopTimeout = 40 * time.Millisecond
	cfg.Worker.ShutdownTimeout = time.Second
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	require.NoError(t, err)
	waitStarted(t, r)
	require.NoError(t, r.Stop(context.Background()))
	result := r.Snapshot()
	require.True(t, result.State == Stopped && result.Cleanup == "complete" && result.Code != "stop_timeout",
		"shutdown inherited stop budget: %+v", result)
}

func TestRuntimeDeadlineReasonSurvivesConfirmedStop(t *testing.T) {
	rt := newRuntime(t, runtimeConfig(t, "stop"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, err := rt.Run(ctx, runtimePlan(t))
	require.NoError(t, err)
	_, result := waitRuntimeRun(t, r)
	require.True(t, result.State == Stopped && result.Code == "deadline_exceeded" && result.Cleanup == "complete",
		"deadline reason = %+v", result)
}

func TestRuntimePersistsSettledTasksBeforeBatchEnd(t *testing.T) {
	cfg := runtimeConfig(t, "partialhold")
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	require.NoError(t, err)
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				require.FailNow(t, fmt.Sprintf("batch terminated before checkpoint: %+v", r.Snapshot()))
			}
			// The monitor processes this event after the preceding task's
			// terminal callback and its synchronous journal update.
			if e.MessageID == 99998 {
				goto persisted
			}
		case <-timer.C:
			require.FailNow(t, "terminal task was not persisted")
		}
	}
persisted:
	record, err := ReadRecord(cfg.DataDir, r.Snapshot().RunID)
	require.NoError(t, err)
	require.True(t, !terminal(record.Result.State) && record.Result.Tasks[0].State == Succeeded && record.Result.Tasks[1].State == Submitted,
		"in-flight record lost confirmed task: %+v", record.Result)
	// Reopen an isolated copy of the crash-time disk snapshot. Recovery must
	// retain confirmed task facts while marking the unfinished batch unknown;
	// opening the runtime itself must not launch or replay the worker.
	recoveryCfg := runtimeConfig(t, "crash")
	s, err := openStore(recoveryCfg.DataDir)
	require.NoError(t, err)
	require.NoError(t, s.save(record))
	require.NoError(t, s.close())
	_ = newRuntime(t, recoveryCfg)
	recovered, err := ReadRecord(recoveryCfg.DataDir, record.Result.RunID)
	require.NoError(t, err)
	require.True(t, recovered.Result.State == Unknown && recovered.Result.Code == "host_interrupted" &&
		recovered.Result.Tasks[0].State == Succeeded && recovered.Result.Tasks[1].State == Unknown,
		"recovery erased settled task: %+v", recovered.Result)
}

func TestRuntimeSettledTaskRecordFailureStopsRun(t *testing.T) {
	cfg := runtimeConfig(t, "terminalwritefail")
	release := filepath.Join(cfg.DataDir, "release")
	cfg.Worker.Env = append(cfg.Worker.Env, "RHODECK_TEST_CONTROL_RELEASE="+release)
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	require.NoError(t, err)
	waitStarted(t, r)
	runs := filepath.Join(cfg.DataDir, "runs")
	require.NoError(t, os.RemoveAll(runs))
	require.NoError(t, os.WriteFile(runs, []byte("blocked"), 0600))
	require.NoError(t, os.WriteFile(release, nil, 0600))
	_, result := waitRuntimeRun(t, r)
	require.True(t, result.State == Unknown && result.Code == "record_write_failed" && result.CleanupCode == "record_write_failed" && result.Tasks[0].State == Succeeded,
		"terminal journal failure was not observed: %+v", result)
}
