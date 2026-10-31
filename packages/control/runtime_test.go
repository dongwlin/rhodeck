package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("RHODECK_TEST_CONTROL_WORKER"); mode != "" {
		os.Exit(controlWorkerHelper(mode))
	}
	os.Exit(m.Run())
}

// The helper is a real child, so deadlines, broken pipes, pending operations,
// process waiting and late cleanup all cross the production supervisor boundary.
func controlWorkerHelper(mode string) int {
	// Tests that verify process reaping record the fixture PID before any other
	// work; a missing marker is a fixture failure, not a pass.
	if path := os.Getenv("RHODECK_TEST_CONTROL_WORKER_PID"); path != "" {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid())), 0600); err != nil {
			return 7
		}
	}
	var sequence, coreSequence uint64
	emit := func(kind, name string, id uint64, body any, cause *protocol.Error) {
		m, err := protocol.NewMessage(kind, name, id, body)
		if err != nil {
			panic(err)
		}
		if kind == protocol.Event {
			sequence++
			m.Sequence = sequence
		}
		m.Error = cause
		b, err := protocol.Encode(m)
		if err != nil {
			panic(err)
		}
		if protocol.WriteFrame(os.Stdout, b) != nil {
			os.Exit(3)
		}
	}
	core := func(id int32, raw string) {
		coreSequence++
		emit(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: id, CoreSequence: coreSequence, JSON: json.RawMessage(raw)}, nil)
	}
	if mode == "nohello" {
		time.Sleep(time.Hour)
		return 1
	}
	emit(protocol.Hello, "", 0, protocol.Greeting{
		WorkerVersion: "control-fixture", SessionID: "control-fixture", OS: "fixture", Arch: "fixture",
		MaxFrameBytes: protocol.MaxFrameBytes, QueueMessages: protocol.QueueMessages, QueueBytes: protocol.QueueBytes,
	}, nil)
	reader := protocol.NewReader(os.Stdin)
	var tasks []int32
	var running bool
	for {
		m, err := reader.Read()
		if err != nil {
			return 2
		}
		respond := func(body any) { emit(protocol.Response, m.Name, m.ID, body, nil) }
		reject := func() {
			emit(protocol.Response, m.Name, m.ID, nil, &protocol.Error{Code: protocol.NativeRejected, Message: "fixture rejected command"})
		}
		switch m.Name {
		case protocol.Initialize:
			if mode == "initreject" {
				reject()
				continue
			}
			respond(protocol.RuntimeInfo{SnapshotID: "fixture", CoreVersion: "fixture-core", LibrarySHA256: strings.Repeat("a", 64),
				Resources: []protocol.Resource{{Root: "fixture-root"}}, Limits: &protocol.Limits{InputJSONBytes: 1 << 20}, NullSize: math.MaxUint64})
		case protocol.Connect:
			if mode == "connectreject" {
				reject()
				continue
			}
			respond(protocol.OperationAccepted{AsyncID: 41})
			if mode != "connectpending" {
				emit(protocol.Event, protocol.OperationEvent, 0, protocol.OperationResult{RequestID: m.ID, AsyncID: 41, What: "Connect", OK: true}, nil)
			}
		case protocol.AppendTask:
			if mode == "appendreject" {
				reject()
				continue
			}
			var task protocol.TaskRequest
			if protocol.Decode(m.Payload, &task) != nil || !strings.Contains(string(task.Params), "9007199254740993") {
				return 4
			}
			id := int32(len(tasks) + 101)
			if mode == "duplicatetaskid" {
				id = 101
			}
			tasks = append(tasks, id)
			respond(protocol.TaskAccepted{TaskID: id})
		case protocol.Start:
			if mode == "starthang" {
				continue
			}
			if mode == "startreject" {
				reject()
				continue
			}
			running = true
			respond(struct{}{})
			core(10001, fmt.Sprintf(`{"taskid":%d,"taskchain":"same"}`, tasks[0]))
			switch mode {
			case "terminalwritefail":
				deadline := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(os.Getenv("RHODECK_TEST_CONTROL_RELEASE")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						return 6
					}
					time.Sleep(time.Millisecond)
				}
				core(10002, fmt.Sprintf(`{"taskid":%d}`, tasks[0]))
				continue
			case "partialhold":
				core(10002, fmt.Sprintf(`{"taskid":%d}`, tasks[0]))
				core(99998, `{"checkpoint":true}`)
				continue
			case "errorstopped", "completedstopped":
				message := int32(10000)
				if mode == "completedstopped" {
					message = 10002
				}
				core(message, fmt.Sprintf(`{"taskid":%d}`, tasks[0]))
				core(10004, fmt.Sprintf(`{"taskid":%d}`, tasks[0]))
				running = false
				continue
			case "crash":
				return 9
			case "stop", "stopwait", "stopstuck", "stopcallback", "finalwritefail", "slowclose":
				continue
			case "overflow":
				for i := 0; i < 200; i++ {
					core(99999, `{"future":9007199254740993}`)
					time.Sleep(100 * time.Microsecond)
				}
				continue
			case "byteoverflow":
				for i := 0; i < 6; i++ {
					core(99999, `{"future":"`+strings.Repeat("x", 3<<20)+`"}`)
					time.Sleep(time.Millisecond)
				}
				continue
			case "foreign":
				core(10002, `{"taskid":999}`)
				continue
			case "malformed":
				core(10002, `{"taskid":101,"taskid":102}`)
				continue
			}
			for i, taskID := range tasks {
				if mode == "suberror" {
					core(20000, fmt.Sprintf(`{"taskid":%d}`, taskID))
				}
				if mode == "failure" && i == 0 {
					core(10000, fmt.Sprintf(`{"taskid":%d}`, taskID))
				} else if mode != "missingcompletion" {
					core(10002, fmt.Sprintf(`{"taskid":%d}`, taskID))
				}
			}
			if mode == "unknowncallback" {
				core(99999, `{"future":9007199254740993,"private":"secret"}`)
			}
			finished := append([]int32(nil), tasks...)
			switch mode {
			case "missingbatchid":
				finished = finished[:len(finished)-1]
			case "extrabatchid":
				finished = append(finished, 999)
			case "duplicatebatchid":
				finished[len(finished)-1] = finished[0]
			}
			b, _ := json.Marshal(finished)
			core(3, `{"finished_tasks":`+string(b)+`}`)
			if mode == "latestopped" {
				core(10004, fmt.Sprintf(`{"taskid":%d}`, tasks[len(tasks)-1]))
			}
			running = false
		case protocol.Stop:
			if mode == "stopstuck" {
				respond(protocol.StopResult{Stopped: false})
			} else if mode == "stopwait" || mode == "stopcallback" {
				respond(protocol.StopResult{Stopped: false})
				if mode == "stopcallback" {
					core(10004, fmt.Sprintf(`{"taskid":%d}`, tasks[0]))
				}
				running = false
			} else {
				running = false
				respond(protocol.StopResult{Stopped: true})
			}
		case protocol.Status:
			respond(protocol.ExecutionStatus{Running: running, Connected: true, Tasks: tasks})
		case protocol.Close:
			if mode == "slowclose" {
				time.Sleep(150 * time.Millisecond)
			}
			emit(protocol.Event, protocol.DiagnosticsEvent, 0, protocol.InstanceDiagnostics{}, nil)
			respond(struct{}{})
			if mode == "closehang" {
				time.Sleep(time.Hour)
			}
			return 0
		default:
			return 5
		}
	}
}

func runtimePlan(t *testing.T) Plan {
	t.Helper()
	root := t.TempDir()
	return Plan{SchemaVersion: SchemaVersion, HostRunID: "host-run", Initialize: protocol.InitializeRequest{
		SnapshotID: "fixture", LibraryPath: filepath.Join(root, "core"), UserDir: filepath.Join(root, "user"), ResourceRoots: []string{root}},
		Connection: protocol.ConnectRequest{ADBPath: filepath.Join(root, "adb"), Address: "fixture"},
		Tasks: []Task{{ID: "first", Type: "FutureTask", Params: json.RawMessage(`{"value":9007199254740993}`)},
			{ID: "second", Type: "FutureTask", Params: json.RawMessage(`{"value":9007199254740993}`)}}}
}

func runtimeConfig(t *testing.T, mode string) Config {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{DataDir: t.TempDir(), StopTimeout: 300 * time.Millisecond,
		Worker: worker.Config{Executable: executable, Env: []string{"RHODECK_TEST_CONTROL_WORKER=" + mode, "GORACE=atexit_sleep_ms=0"},
			StartupTimeout: time.Second, RequestTimeout: 300 * time.Millisecond, ShutdownTimeout: 300 * time.Millisecond}}
}

func newRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	rt, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := rt.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return rt
}

func waitRuntimeRun(t *testing.T, r *Run) ([]Event, Result) {
	t.Helper()
	var events []Event
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-r.Events():
			if ok {
				events = append(events, e)
			} else {
				<-r.Done()
				return events, r.Snapshot()
			}
		case <-timer.C:
			t.Fatal("run did not terminate")
		}
	}
}

func waitStarted(t *testing.T, r *Run) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				t.Fatalf("run terminated before start: %+v", r.Snapshot())
			}
			if e.Type == "state" && e.State == Started {
				return
			}
		case <-timer.C:
			t.Fatal("run did not start")
		}
	}
}

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
			if err != nil {
				t.Fatal(err)
			}
			events, result := waitRuntimeRun(t, r)
			if result.State != tc.state || result.Cleanup != tc.clean {
				t.Fatalf("result = %+v, want %s/%s", result, tc.state, tc.clean)
			}
			for i, e := range events {
				if e.Sequence != uint64(i+1) || e.RunID != result.RunID || e.HostRunID != "host-run" || e.SchemaVersion != SchemaVersion {
					t.Fatalf("invalid event correlation: %+v", e)
				}
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			if err != nil || record.Result.State != result.State || record.Result.Cleanup != result.Cleanup {
				t.Fatalf("record = %+v, %v", record.Result, err)
			}
			if tc.mode == "failure" && (result.Tasks[0].State != Failed || result.Tasks[1].State != Succeeded) {
				t.Fatalf("partial task facts lost: %+v", result.Tasks)
			}
			if tc.mode == "errorstopped" && (result.Tasks[0].State != Failed || result.Tasks[1].State != Unknown) {
				t.Fatalf("error followed by stop lost task facts: %+v", result.Tasks)
			}
			if tc.mode == "completedstopped" && (result.Tasks[0].State != Succeeded || result.Tasks[1].State != Stopped) {
				t.Fatalf("completion followed by stop lost task facts: %+v", result.Tasks)
			}
			if tc.mode == "closehang" && result.Tasks[0].State != Succeeded {
				t.Fatalf("cleanup rewrote task facts: %+v", result.Tasks)
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
			if err != nil {
				t.Fatal(err)
			}
			waitStarted(t, r)
			cancel()
			_, result := waitRuntimeRun(t, r)
			want := Stopped
			if mode == "stopstuck" {
				want = Unknown
			}
			if result.State != want {
				t.Fatalf("result = %+v, want %s", result, want)
			}
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
			if err != nil {
				t.Fatal(err)
			}
			// The byte-overflow fixture delivers six 3 MiB callbacks, so decoding
			// them dominates wall time under the race detector; cleanup itself
			// finishes within milliseconds. This bound only has to distinguish
			// "cleanup finished" from "cleanup waits for the consumer forever".
			select {
			case <-r.Done():
			case <-time.After(60 * time.Second):
				t.Fatal("cleanup depended on event consumer")
			}
			if result := r.Snapshot(); result.State != Unknown || result.Code != "event_overflow" {
				t.Fatalf("result = %+v", result)
			}
			count := 0
			for range r.Events() {
				count++
			}
			if count > runEventMessages || count == 0 {
				t.Fatalf("retained events = %d", count)
			}
		})
	}
}

func TestRuntimeConcurrentStopCloseAndAdmission(t *testing.T) {
	cfg := runtimeConfig(t, "stop")
	rt := newRuntime(t, cfg)
	plan := runtimePlan(t)
	r, err := rt.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), plan); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent run = %v", err)
	}
	if _, err := New(cfg); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent directory owner = %v", err)
	}
	waitStarted(t, r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if err := r.Stop(context.Background()); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if err := rt.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if result := r.Snapshot(); result.State != Stopped || result.Cleanup != "complete" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := rt.Run(context.Background(), plan); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed runtime admission = %v", err)
	}
	if next, err := New(cfg); err != nil {
		t.Fatalf("directory was not released: %v", err)
	} else {
		_ = next.Close(context.Background())
	}
}

func TestRuntimeUnknownCallbackAndSnapshotOwnership(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			cfg := runtimeConfig(t, "unknowncallback")
			cfg.IncludeRawEvents = raw
			rt := newRuntime(t, cfg)
			plan := runtimePlan(t)
			r, err := rt.Run(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			plan.Tasks[0].Params[0] = '['
			plan.Initialize.ResourceRoots[0] = "mutated"
			events, result := waitRuntimeRun(t, r)
			found := false
			for _, e := range events {
				if e.MessageID != 99999 {
					continue
				}
				found = true
				if raw != (len(e.Details) != 0) || (raw && !strings.Contains(string(e.Details), "9007199254740993")) {
					t.Fatalf("raw event policy lost: %+v", e)
				}
			}
			if !found || result.State != Succeeded {
				t.Fatalf("result = %+v, unknown callback present = %t", result, found)
			}
			result.Tasks[0].State = Failed
			result.RuntimeInfo.Resources[0].Root = "mutated"
			result.RuntimeInfo.Limits.InputJSONBytes = 1
			next := r.Snapshot()
			if next.Tasks[0].State != Succeeded || next.RuntimeInfo.Resources[0].Root == "mutated" || next.RuntimeInfo.Limits.InputJSONBytes == 1 {
				t.Fatalf("mutable snapshot alias: %+v", next)
			}
		})
	}
}

func TestRuntimeCancelledCloseKeepsOwnershipUntilCleanup(t *testing.T) {
	cfg := runtimeConfig(t, "stopwait")
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := r.Snapshot(); result.State != Stopped || result.Cleanup != "complete" {
		t.Fatalf("background cleanup = %+v", result)
	}
}

func TestRuntimeFinalRecordWriteFailureIsVisible(t *testing.T) {
	cfg := runtimeConfig(t, "finalwritefail")
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, r)
	runs := filepath.Join(cfg.DataDir, "runs")
	if err := os.RemoveAll(runs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runs, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(context.Background()); err == nil {
		t.Fatal("record failure was reported as confirmed cleanup")
	}
	if result := r.Snapshot(); result.State != Unknown || result.CleanupCode != "record_write_failed" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRuntimeStopAndShutdownHaveIndependentBudgets(t *testing.T) {
	cfg := runtimeConfig(t, "slowclose")
	cfg.StopTimeout = 40 * time.Millisecond
	cfg.Worker.ShutdownTimeout = time.Second
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, r)
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := r.Snapshot(); result.State != Stopped || result.Cleanup != "complete" || result.Code == "stop_timeout" {
		t.Fatalf("shutdown inherited stop budget: %+v", result)
	}
}

func TestRuntimeDeadlineReasonSurvivesConfirmedStop(t *testing.T) {
	rt := newRuntime(t, runtimeConfig(t, "stop"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r, err := rt.Run(ctx, runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	_, result := waitRuntimeRun(t, r)
	if result.State != Stopped || result.Code != "deadline_exceeded" || result.Cleanup != "complete" {
		t.Fatalf("deadline reason = %+v", result)
	}
}

func TestRuntimePersistsSettledTasksBeforeBatchEnd(t *testing.T) {
	cfg := runtimeConfig(t, "partialhold")
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				t.Fatalf("batch terminated before checkpoint: %+v", r.Snapshot())
			}
			// The monitor processes this event after the preceding task's
			// terminal callback and its synchronous journal update.
			if e.MessageID == 99998 {
				goto persisted
			}
		case <-timer.C:
			t.Fatal("terminal task was not persisted")
		}
	}
persisted:
	record, err := ReadRecord(cfg.DataDir, r.Snapshot().RunID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal(record.Result.State) || record.Result.Tasks[0].State != Succeeded || record.Result.Tasks[1].State != Submitted {
		t.Fatalf("in-flight record lost confirmed task: %+v", record.Result)
	}
	// Reopen an isolated copy of the crash-time disk snapshot. Recovery must
	// retain confirmed task facts while marking the unfinished batch unknown;
	// opening the runtime itself must not launch or replay the worker.
	recoveryCfg := runtimeConfig(t, "crash")
	s, err := openStore(recoveryCfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	_ = newRuntime(t, recoveryCfg)
	recovered, err := ReadRecord(recoveryCfg.DataDir, record.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Result.State != Unknown || recovered.Result.Code != "host_interrupted" ||
		recovered.Result.Tasks[0].State != Succeeded || recovered.Result.Tasks[1].State != Unknown {
		t.Fatalf("recovery erased settled task: %+v", recovered.Result)
	}
}

func TestRuntimeSettledTaskRecordFailureStopsRun(t *testing.T) {
	cfg := runtimeConfig(t, "terminalwritefail")
	release := filepath.Join(cfg.DataDir, "release")
	cfg.Worker.Env = append(cfg.Worker.Env, "RHODECK_TEST_CONTROL_RELEASE="+release)
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	waitStarted(t, r)
	runs := filepath.Join(cfg.DataDir, "runs")
	if err := os.RemoveAll(runs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runs, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, result := waitRuntimeRun(t, r)
	if result.State != Unknown || result.Code != "record_write_failed" || result.CleanupCode != "record_write_failed" || result.Tasks[0].State != Succeeded {
		t.Fatalf("terminal journal failure was not observed: %+v", result)
	}
}
