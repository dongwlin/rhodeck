// Shared fixtures for the root package tests; no production code under test.

package control

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// The helper is a real child, so deadlines, broken pipes, pending operations,
// process waiting and late cleanup all cross the production supervisor boundary.
func controlWorkerHelper(mode string) int {
	// Tests that verify process reaping record the fixture PID before any other
	// work; a missing marker is a fixture failure, not a pass.
	if path := os.Getenv("RHODECK_TEST_CONTROL_WORKER_PID"); path != "" {
		if err := os.WriteFile(path, fmt.Appendf(nil, "%d", os.Getpid()), 0600); err != nil {
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
	require.NoError(t, err)
	return Config{DataDir: t.TempDir(), StopTimeout: 300 * time.Millisecond,
		Worker: worker.Config{Executable: executable, Env: []string{"RHODECK_TEST_CONTROL_WORKER=" + mode, "GORACE=atexit_sleep_ms=0"},
			StartupTimeout: time.Second, RequestTimeout: 300 * time.Millisecond, ShutdownTimeout: 300 * time.Millisecond}}
}

func newRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	rt, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		assert.NoError(t, rt.Close(ctx))
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
			require.FailNow(t, "run did not terminate")
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
				require.FailNow(t, fmt.Sprintf("run terminated before start: %+v", r.Snapshot()))
			}
			if e.Type == "state" && e.State == Started {
				return
			}
		case <-timer.C:
			require.FailNow(t, "run did not start")
		}
	}
}

func validTestPlan(t *testing.T) Plan {
	t.Helper()
	dir := t.TempDir()
	return Plan{SchemaVersion: SchemaVersion, HostRunID: "external-run", Initialize: protocol.InitializeRequest{
		SnapshotID: "test-install", LibraryPath: filepath.Join(dir, "MaaCore"), UserDir: dir, ResourceRoots: []string{dir},
	}, Connection: protocol.ConnectRequest{ADBPath: filepath.Join(dir, "adb"), Address: "fixture"}, Tasks: []Task{{ID: "task-1", Type: "FutureTask", Params: json.RawMessage(`{"large":9007199254740993,"unknown":{"a":1}}`)}}}
}

// requireWorkerProcessExited proves the fixture worker process is gone after the
// runtime reported Done and cleanup. A Unix signal-0 probe reports whether the
// PID still exists; Windows cannot probe an unrelated process this way, so the
// direct liveness check is skipped there and the runtime cleanup assertions
// stand.
func requireWorkerProcessExited(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "worker PID marker missing: %v", err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err, "invalid worker PID marker %q", raw)
	require.Greater(t, pid, 0, "invalid worker PID marker %q", raw)
	if runtime.GOOS == "windows" {
		t.Logf("worker %d liveness probe skipped on windows", pid)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	require.Error(t, proc.Signal(syscall.Signal(0)), "worker process %d was not reaped", pid)
}

// newPreparedTypedHarness drives the real monitor path (Run.observe and
// Run.observeTyped) against prepared adapters without launching a worker. It is
// the same harness shape used by the adapter runtime tests, with the Start flag
// left adjustable so early-callback rejection can be exercised.
func newPreparedTypedHarness(t *testing.T, plan Plan, coreIDs []int32, startIssued bool, adapters ...TaskAdapter) *Run {
	t.Helper()
	prepared, err := PreparePlan(plan, adapters...)
	require.NoError(t, err)
	require.Len(t, coreIDs, len(prepared.Tasks), "got %d Core task IDs for %d tasks", len(coreIDs), len(prepared.Tasks))
	s, err := openStore(t.TempDir())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.close()
	})
	index := make(map[int32]int, len(coreIDs))
	for i, id := range coreIDs {
		prepared.Tasks[i].CoreTaskID = id
		prepared.Tasks[i].State = Started
		index[id] = i
	}
	return &Run{
		runtime:        &Runtime{store: s},
		plan:           prepared.Plan,
		sourcePlan:     clonePlan(plan),
		result:         Result{SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending", Tasks: prepared.Tasks},
		events:         make(chan Event, runEventMessages),
		done:           make(chan struct{}),
		changed:        make(chan struct{}, 1),
		commands:       ctx,
		cancelCommands: cancel,
		taskIndex:      index,
		reducers:       prepared.reducers,
		startIssued:    startIssued,
	}
}

func typedEventsIn(r *Run) []Event {
	var events []Event
	for {
		select {
		case event := <-r.events:
			if event.Type == "typed" {
				events = append(events, event)
			}
		default:
			return events
		}
	}
}

func sendTypedCallback(t *testing.T, r *Run, messageID int32, raw string) {
	t.Helper()
	m, err := protocol.NewMessage(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: messageID, CoreSequence: 1, JSON: json.RawMessage(raw)})
	require.NoError(t, err)
	r.observe(m)
}
