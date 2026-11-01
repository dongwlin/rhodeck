package control

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

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

func copilotPreparationCallback(subtask, what, why string) string {
	details := json.RawMessage(`{}`)
	switch what {
	case "CopilotPrecheckFile":
		details = json.RawMessage(`{"nav_name":"Example Stage","filename":"same.json"}`)
	case "BattleFormationOperboxMatched":
		details = json.RawMessage(`{"matched_groups":[{"group_name":"Example Group","oper_name":"Example Operator"}]}`)
	case "BattleFormationOperbox1Unmatched":
		details = json.RawMessage(`{"group_name":"Example Group","may_borrow_oper":"Example Support"}`)
	case "UserAdditionalOperInvalid":
		details = json.RawMessage(`{"name":"Example Operator"}`)
	case "UnsupportedLevel":
		details = json.RawMessage(`{"level":"example-stage"}`)
	case "CopilotStageNotSupported":
		details = json.RawMessage(`{"stage_name":"example-stage"}`)
	}
	if why == "OperatorMissing" {
		details = json.RawMessage(`{"opers":{"Example Group":[{"name":"Example Operator","reason":"Missing"}]}}`)
	}
	raw, _ := json.Marshal(struct {
		TaskID    int32           `json:"taskid"`
		TaskChain string          `json:"taskchain"`
		Subtask   string          `json:"subtask"`
		What      string          `json:"what,omitempty"`
		Why       string          `json:"why,omitempty"`
		Details   json.RawMessage `json:"details"`
	}{0, "Copilot", subtask, what, why, details})
	return string(raw)
}

func TestCopilotPreparationCallbacksBeforeAppendResponse(t *testing.T) {
	for _, params := range []string{
		`{"copilot_list":[{"id":7,"filename":"same.json"}],"formation":true,"operbox_data_path":"box.json"}`,
		`{"filename":"same.json","formation":true,"operbox_data_path":"box.json"}`,
	} {
		t.Run(params, func(t *testing.T) {
			cfg := runtimeConfig(t, "copilot-preparation")
			cfg.Adapters = []TaskAdapter{NewCopilotAdapter()}
			rt := newRuntime(t, cfg)
			run, err := rt.Run(context.Background(), copilotRuntimePlan(t, params))
			if err != nil {
				t.Fatal(err)
			}
			events, result := waitRuntimeRun(t, run)
			if result.State != Succeeded || result.Cleanup != "complete" {
				t.Fatalf("preparation callbacks prevented accepted work: %+v", result)
			}
			prechecks, typed := 0, 0
			for _, event := range events {
				if event.Type == "core" && event.MessageID == 20003 && event.CoreTaskID == 0 {
					prechecks++
					if event.TaskID != "" {
						t.Fatalf("preparation observation gained a task association: %+v", event)
					}
				}
				if event.Type == "typed" {
					typed++
					if event.MessageID != 20001 || event.CoreTaskID != 101 || event.Typed.Targets[0].Outcome != TargetAchieved {
						t.Fatalf("preparation observation became a business fact: %+v", event)
					}
				}
			}
			wantTyped := 1
			if strings.Contains(params, `"filename":"same.json","formation"`) {
				wantTyped = 0
				if result.Tasks[0].Typed != nil || result.Tasks[0].Policy != ObjectiveNone {
					t.Fatalf("single-file preparation gained objectives: %+v", result.Tasks[0])
				}
			}
			if prechecks != 3 || typed != wantTyped {
				t.Fatalf("preparation/typed events = %d/%d, want 3/%d", prechecks, typed, wantTyped)
			}
		})
	}
}

func TestCopilotPreparationErrorsLeaveAppendResponseAuthoritative(t *testing.T) {
	for _, reason := range []string{"OperatorMissing", "OperboxDataParseFailed", "CopilotFileReadError", "UserAdditionalOperInvalid", "CopilotStageNotSupported", "UnsupportedLevel"} {
		t.Run(reason, func(t *testing.T) {
			cfg := runtimeConfig(t, "copilot-preparation")
			cfg.Worker.Env = append(cfg.Worker.Env, "RHODECK_TEST_COPILOT_PREPARATION_REJECT="+reason)
			cfg.Adapters = []TaskAdapter{NewCopilotAdapter()}
			rt := newRuntime(t, cfg)
			run, err := rt.Run(context.Background(), copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			events, result := waitRuntimeRun(t, run)
			if result.State != Rejected || result.Cleanup != "complete" || result.Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved {
				t.Fatalf("preparation error replaced append rejection: %+v", result)
			}
			for _, event := range events {
				if event.Type == "typed" || event.Typed != nil {
					t.Fatalf("preparation error manufactured a target failure: %+v", event)
				}
			}
		})
	}
}

func TestCopilotPreparationErrorsInMixedInputOnlyTask(t *testing.T) {
	for _, reason := range []string{"CopilotStageNotSupported", "UnsupportedLevel"} {
		t.Run(reason, func(t *testing.T) {
			cfg := runtimeConfig(t, "copilot-preparation")
			cfg.Worker.Env = append(cfg.Worker.Env, "RHODECK_TEST_COPILOT_PREPARATION_REJECT="+reason, "RHODECK_TEST_COPILOT_PREPARATION_REJECT_AT=2")
			cfg.Adapters = []TaskAdapter{NewCopilotAdapter()}
			rt := newRuntime(t, cfg)
			plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"}]}`, `{"filename":"same.json"}`)
			run, err := rt.Run(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			events, result := waitRuntimeRun(t, run)
			if result.State != Rejected || result.Cleanup != "complete" || result.Tasks[0].CoreTaskID != 101 || result.Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved || result.Tasks[1].Policy != ObjectiveNone || result.Tasks[1].Typed != nil {
				t.Fatalf("input-only preparation error polluted typed task routing: %+v", result)
			}
			for _, event := range events {
				if event.Type == "typed" || event.Typed != nil {
					t.Fatalf("input-only rejection created a target fact: %+v", event)
				}
			}
		})
	}
}

func TestCopilotPreparationLateProgressPreservesActiveEntryScope(t *testing.T) {
	plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"}]}`)
	run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
	sendTypedCallback(t, run, 20003, copilotRuntimeLoad(101, 7, "same.json"))
	for _, callback := range []string{
		copilotPreparationCallback("CopilotTask", "CopilotPrecheckFile", ""),
		copilotPreparationCallback("BattleFormationTask", "BattleFormationOperboxMatched", ""),
		copilotPreparationCallback("BattleFormationTask", "BattleFormationOperbox1Unmatched", ""),
	} {
		sendTypedCallback(t, run, 20003, callback)
	}
	if run.fatal != "" || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved || len(typedEventsIn(run)) != 0 {
		t.Fatalf("late preparation progress became a target fact: fatal=%s, result=%+v", run.fatal, run.Snapshot())
	}
	sendTypedCallback(t, run, 20001, copilotRuntimeNode(101, "StageDrops-Stars-3"))
	if run.fatal != "" || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || len(typedEventsIn(run)) != 1 {
		t.Fatalf("late preparation progress cleared entry scope: fatal=%s, result=%+v", run.fatal, run.Snapshot())
	}
}

func TestCopilotPreparationExceptionRejectsUnassociatedEvidence(t *testing.T) {
	valid := copilotPreparationCallback("CopilotTask", "CopilotPrecheckFile", "")
	for _, test := range []struct {
		name      string
		messageID int32
		raw       string
	}{
		{"duplicate correlation", 20003, strings.Replace(valid, `"taskid":0`, `"taskid":0,"taskid":101`, 1)},
		{"duplicate nested field", 20003, mutateCopilotResultField(t, valid, "details", `{"filename":"same.json","filename":"other.json"}`)},
		{"wrong subtask", 20003, copilotPreparationCallback("ProcessTask", "CopilotPrecheckFile", "")},
		{"unknown preparation event", 20003, copilotPreparationCallback("CopilotTask", "FuturePrecheck", "")},
		{"missing id", 20003, mutateCopilotResultField(t, valid, "taskid", "")},
		{"foreign nonzero id", 20003, mutateCopilotResultField(t, valid, "taskid", "999")},
		{"null id", 20003, mutateCopilotResultField(t, valid, "taskid", "null")},
		{"missing details", 20003, mutateCopilotResultField(t, valid, "details", "")},
		{"zero id load", 20003, copilotRuntimeLoad(0, 7, "same.json")},
		{"zero id settlement", 20001, copilotRuntimeNode(0, "StageDrops-Stars-3")},
		{"missing id settlement", 20001, strings.Replace(copilotRuntimeNode(101, "StageDrops-Stars-3"), `"taskid":101,`, "", 1)},
		{"foreign id settlement", 20001, copilotRuntimeNode(999, "FightMissionFailed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"}]}`)
			run := newPreparedTypedHarness(t, plan, []int32{101}, false, NewCopilotAdapter())
			sendTypedCallback(t, run, test.messageID, test.raw)
			if run.fatal != "typed_invalid_event" || run.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetUnobserved || len(typedEventsIn(run)) != 0 {
				t.Fatalf("invalid pre-acceptance evidence was admitted: fatal=%s, result=%+v", run.fatal, run.Snapshot())
			}
		})
	}
}
