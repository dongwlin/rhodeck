package control_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("RHODECK_TEST_CONTROL_WORKER"); mode != "" {
		if mode == "copilot-preparation" {
			os.Exit(copilotPreparationWorkerHelper())
		}
		os.Exit(ControlWorkerForTest(mode))
	}
	os.Exit(m.Run())
}

// These values are callback and output contract fixtures, independent of adapter internals.
const (
	operProgressTaskType                      = "OperProgress"
	operProgressAdapterName                   = "oper_progress"
	operProgressVersion                       = 1
	operProgressMaxPlans                      = MaxTypedTargetsPerTask
	operProgressMessageSubTaskExtraInfo int32 = 20003
	operProgressWhatDetail                    = "OperProgressDetail"
	operProgressWhatSummary                   = "OperProgressSummary"
	switchThemeTaskType                       = "SwitchTheme"
	switchThemeAdapterName                    = "switch_theme"
	switchThemeVersion                        = 1
	switchThemeMessageSubTaskCompleted  int32 = 20002
	switchThemeNodeAlreadySet                 = "SwitchThemeByNameAlreadySet"
	switchThemeNodeLocked                     = "SwitchThemeByNameLockedTheme"
	switchThemeNodeConfirm                    = "SwitchThemeByNameConfirmTheme"
	switchThemeCodeAlreadySet                 = "already_set"
	switchThemeCodeLocked                     = "locked"
	switchThemeCodeConfirmationObserved       = "confirmation_observed"
	roguelikeTargetInvestment                 = "roguelike:investment"
	roguelikeTargetMonthlySquad               = "roguelike:monthly_squad"
	roguelikeTargetDeepExploration            = "roguelike:deep_exploration"
	roguelikeTargetJieGarden                  = "roguelike:jie_garden_target"
	roguelikeTargetStartRun                   = "roguelike:start_run"
	roguelikeCodeGamePass                     = "game_pass"
	roguelikeCodeMissionFailed                = "mission_failed"
	roguelikeCodeInvestmentLimit              = "investment_limit_reached"
	roguelikeCodeInvestmentFull               = "investment_full"
	roguelikeCodeRotationDone                 = "rotation_completed"
	roguelikeCodeTargetFound                  = "target_found"
	roguelikeCodeStartRunDone                 = "start_run_completed"
	roguelikeWhatInvestmentLimit              = "RoguelikeInvestmentReachLimit"
	roguelikeWhatInvestmentFull               = "RoguelikeInvestmentReachFull"
	roguelikeWhatMonthlySquad                 = "MonthlySquadCompleted"
	roguelikeWhatDeepExploration              = "DeepExplorationCompleted"
	roguelikeWhatJieGardenFound               = "RoguelikeJieGardenTargetFound"
	roguelikeWhatStartRun                     = "BlackFlowStartRun"
)

var (
	applyTypedUpdate        = ApplyTypedUpdateForTest
	validateTypedResult     = ValidateTypedResultForTest
	typedState              = TypedStateForTest
	newPreparedTypedHarness = NewPreparedRunForTest
)

var sameTarget = SameTargetForTest

func roguelikeRunTargetID(run int) string { return fmt.Sprintf("roguelike:run:%d", run) }
func roguelikeTask(params string) Task {
	return Task{ID: "roguelike-task", Type: "Roguelike", Params: json.RawMessage(params)}
}

type runtimeReducerFunc func(Observation) (TypedUpdate, error)

func (f runtimeReducerFunc) Observe(o Observation) (TypedUpdate, error) { return f(o) }

func sendTypedCallback(t *testing.T, r *Run, messageID int32, raw string) {
	t.Helper()
	m, err := protocol.NewMessage(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: messageID, CoreSequence: 1, JSON: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	ObserveRunForTest(r, m)
}

func typedEventsIn(r *Run) []Event {
	var events []Event
	for {
		select {
		case event := <-r.Events():
			if event.Type == "typed" {
				events = append(events, event)
			}
		default:
			return events
		}
	}
}

func openStore(dir string) (*RecordStoreForTest, error) { return OpenRecordStoreForTest(dir) }
func newRunID() string                                  { return NewRunIDForTest() }
func clonePlan(p Plan) Plan                             { return ClonePlanForTest(p) }
func terminal(s State) bool                             { return TerminalStateForTest(s) }

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

func validTestPlan(t *testing.T) Plan {
	t.Helper()
	dir := t.TempDir()
	return Plan{SchemaVersion: SchemaVersion, HostRunID: "external-run", Initialize: protocol.InitializeRequest{
		SnapshotID: "test-install", LibraryPath: filepath.Join(dir, "MaaCore"), UserDir: dir, ResourceRoots: []string{dir},
	}, Connection: protocol.ConnectRequest{ADBPath: filepath.Join(dir, "adb"), Address: "fixture"}, Tasks: []Task{{ID: "task-1", Type: "FutureTask", Params: json.RawMessage(`{"large":9007199254740993,"unknown":{"a":1}}`)}}}
}

func operProgressTask(params string) Task {
	return Task{ID: "grow", Type: operProgressTaskType, Params: json.RawMessage(params)}
}

func operProgressPlansJSON(n int) string {
	plans := make([]string, n)
	for i := range plans {
		plans[i] = fmt.Sprintf(`{"role":"Warrior","name":"operator-%03d","elite":1}`, i)
	}
	return `{"plans":[` + strings.Join(plans, ",") + `]}`
}

func mutateCopilotResultField(t *testing.T, source, field, value string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &fields); err != nil {
		t.Fatal(err)
	}
	if value == "" {
		delete(fields, field)
	} else {
		fields[field] = json.RawMessage(value)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func requireWorkerProcessExited(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("worker PID marker missing: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid worker PID marker %q", raw)
	}
	if runtime.GOOS == "windows" {
		t.Logf("worker %d liveness probe skipped on windows", pid)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Fatalf("worker process %d was not reaped", pid)
	}
}

func switchThemeThemesJSON(t *testing.T, themes ...string) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Themes []string `json:"themes"`
	}{themes})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func switchThemeSelectCallback(t *testing.T, taskID int32, theme string) string {
	t.Helper()
	encoded, err := json.Marshal(theme)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":%s,"rect":[1,2,3,4],"score":0.98}}}`, taskID, encoded)
}

func switchThemeBranchCallback(t *testing.T, taskID int32, node string) string {
	t.Helper()
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":%s,"result":{"text":"branch","rect":[5,6,7,8],"score":0.5}}}`, taskID, encoded)
}
