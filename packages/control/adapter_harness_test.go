// Shared fixtures for the external adapter tests; no production code under test.

package control_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/dongwlin/rhodeck/packages/control"
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

// The root package owns these harnesses in its test build and exposes them
// through export_test.go. Re-declaring them here would let the two test
// packages drift apart.
var (
	runtimePlan                = RuntimePlanForTest
	runtimeConfig              = RuntimeConfigForTest
	newRuntime                 = NewRuntimeForTest
	waitRuntimeRun             = WaitRuntimeRunForTest
	waitStarted                = WaitStartedForTest
	validTestPlan              = ValidPlanForTest
	requireWorkerProcessExited = RequireWorkerProcessExitedForTest
	sendTypedCallback          = SendTypedCallbackForTest
	typedEventsIn              = TypedEventsInForTest
	sameTarget                 = SameTargetForTest
)

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

func roguelikeRunTargetID(run int) string { return fmt.Sprintf("roguelike:run:%d", run) }

func roguelikeTask(params string) Task {
	return Task{ID: "roguelike-task", Type: "Roguelike", Params: json.RawMessage(params)}
}

type runtimeReducerFunc func(Observation) (TypedUpdate, error)

func (f runtimeReducerFunc) Observe(o Observation) (TypedUpdate, error) { return f(o) }

func openStore(dir string) (*RecordStoreForTest, error) { return OpenRecordStoreForTest(dir) }

func newRunID() string { return NewRunIDForTest() }

func clonePlan(p Plan) Plan { return ClonePlanForTest(p) }

func terminal(s State) bool { return TerminalStateForTest(s) }

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
	err := json.Unmarshal([]byte(source), &fields)
	require.NoError(t, err)
	if value == "" {
		delete(fields, field)
	} else {
		fields[field] = json.RawMessage(value)
	}
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return string(raw)
}

func switchThemeThemesJSON(t *testing.T, themes ...string) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Themes []string `json:"themes"`
	}{themes})
	require.NoError(t, err)
	return string(raw)
}

func switchThemeSelectCallback(t *testing.T, taskID int32, theme string) string {
	t.Helper()
	encoded, err := json.Marshal(theme)
	require.NoError(t, err)
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":%s,"rect":[1,2,3,4],"score":0.98}}}`, taskID, encoded)
}

func switchThemeBranchCallback(t *testing.T, taskID int32, node string) string {
	t.Helper()
	encoded, err := json.Marshal(node)
	require.NoError(t, err)
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":%s,"result":{"text":"branch","rect":[5,6,7,8],"score":0.5}}}`, taskID, encoded)
}

// Copilot validates local files and formation inputs inside AppendTask, before
// it can return an accepted task ID. These callbacks cross a real child pipe so
// their ordering relative to the response is part of the regression test.
func copilotPreparationWorkerHelper() int {
	var eventSequence, coreSequence uint64
	emit := func(kind, name string, requestID uint64, payload any, failure *protocol.Error) {
		message, err := protocol.NewMessage(kind, name, requestID, payload)
		if err != nil {
			panic(err)
		}
		if kind == protocol.Event {
			eventSequence++
			message.Sequence = eventSequence
		}
		message.Error = failure
		frame, err := protocol.Encode(message)
		if err != nil || protocol.WriteFrame(os.Stdout, frame) != nil {
			os.Exit(3)
		}
	}
	core := func(messageID int32, raw string) {
		coreSequence++
		emit(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: messageID, CoreSequence: coreSequence, JSON: json.RawMessage(raw)}, nil)
	}
	emit(protocol.Hello, "", 0, protocol.Greeting{
		WorkerVersion: "copilot-preparation-fixture", SessionID: "copilot-preparation-fixture", OS: "fixture", Arch: "fixture",
		MaxFrameBytes: protocol.MaxFrameBytes, QueueMessages: protocol.QueueMessages, QueueBytes: protocol.QueueBytes,
	}, nil)
	reader := protocol.NewReader(os.Stdin)
	var tasks []int32
	appendCount := 0
	for {
		message, err := reader.Read()
		if err != nil {
			return 2
		}
		respond := func(payload any) { emit(protocol.Response, message.Name, message.ID, payload, nil) }
		switch message.Name {
		case protocol.Initialize:
			respond(protocol.RuntimeInfo{SnapshotID: "fixture", CoreVersion: "fixture-core", LibrarySHA256: strings.Repeat("a", 64),
				Resources: []protocol.Resource{{Root: "fixture-root"}}, Limits: &protocol.Limits{InputJSONBytes: 1 << 20}, NullSize: math.MaxUint64})
		case protocol.Connect:
			respond(protocol.OperationAccepted{AsyncID: 41})
			emit(protocol.Event, protocol.OperationEvent, 0, protocol.OperationResult{RequestID: message.ID, AsyncID: 41, What: "Connect", OK: true}, nil)
		case protocol.AppendTask:
			appendCount++
			core(20003, copilotPreparationCallback("CopilotTask", "CopilotPrecheckFile", ""))
			core(20003, copilotPreparationCallback("BattleFormationTask", "BattleFormationOperboxMatched", ""))
			core(20003, copilotPreparationCallback("BattleFormationTask", "BattleFormationOperbox1Unmatched", ""))
			rejectAt := os.Getenv("RHODECK_TEST_COPILOT_PREPARATION_REJECT_AT")
			if reason := os.Getenv("RHODECK_TEST_COPILOT_PREPARATION_REJECT"); reason != "" && (rejectAt != "2" || appendCount == 2) {
				subtask, what, why, messageID := "CopilotTask", reason, "", int32(20000)
				if reason == "OperatorMissing" {
					subtask, what, why = "BattleFormationTask", "", reason
				} else if reason == "UnsupportedLevel" {
					subtask, messageID = "BattleProcessTask", 20003
				}
				core(messageID, copilotPreparationCallback(subtask, what, why))
				emit(protocol.Response, message.Name, message.ID, nil, &protocol.Error{Code: protocol.NativeRejected, Message: "fixture rejected preparation"})
				continue
			}
			id := int32(len(tasks) + 101)
			tasks = append(tasks, id)
			respond(protocol.TaskAccepted{TaskID: id})
		case protocol.Start:
			respond(struct{}{})
			for _, id := range tasks {
				core(10001, fmt.Sprintf(`{"taskid":%d,"taskchain":"Copilot"}`, id))
				core(20003, copilotRuntimeLoad(id, 7, "same.json"))
				core(20001, copilotRuntimeNode(id, "StageDrops-Stars-3"))
				core(10002, fmt.Sprintf(`{"taskid":%d,"taskchain":"Copilot"}`, id))
			}
			finished, _ := json.Marshal(tasks)
			core(3, `{"finished_tasks":`+string(finished)+`}`)
		case protocol.Status:
			respond(protocol.ExecutionStatus{Connected: true, Tasks: tasks})
		case protocol.Stop:
			respond(protocol.StopResult{Stopped: true})
		case protocol.Close:
			emit(protocol.Event, protocol.DiagnosticsEvent, 0, protocol.InstanceDiagnostics{}, nil)
			respond(struct{}{})
			return 0
		default:
			return 5
		}
	}
}
