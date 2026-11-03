package roguelike

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

const (
	roguelikeMessageSubTaskStart     int32 = 20001
	roguelikeMessageSubTaskExtraInfo int32 = 20003
	roguelikeSubtaskProcessTask            = "ProcessTask"
)

const (
	roguelikeTargetInvestment      = "roguelike:investment"
	roguelikeTargetMonthlySquad    = "roguelike:monthly_squad"
	roguelikeTargetDeepExploration = "roguelike:deep_exploration"
	roguelikeTargetJieGarden       = "roguelike:jie_garden_target"
	roguelikeTargetStartRun        = "roguelike:start_run"
)

const (
	roguelikeCodeGamePass        = "game_pass"
	roguelikeCodeMissionFailed   = "mission_failed"
	roguelikeCodeInvestmentLimit = "investment_limit_reached"
	roguelikeCodeInvestmentFull  = "investment_full"
	roguelikeCodeRotationDone    = "rotation_completed"
	roguelikeCodeTargetFound     = "target_found"
	roguelikeCodeStartRunDone    = "start_run_completed"
)

const (
	roguelikeNodeGamePass           = "GamePass"
	roguelikeNodeMissionFailedFlag2 = "MissionFailedFlag2"
)

const (
	roguelikeWhatInvestmentLimit = "RoguelikeInvestmentReachLimit"
	roguelikeWhatInvestmentFull  = "RoguelikeInvestmentReachFull"
	roguelikeWhatMonthlySquad    = "MonthlySquadCompleted"
	roguelikeWhatDeepExploration = "DeepExplorationCompleted"
	roguelikeWhatJieGardenFound  = "RoguelikeJieGardenTargetFound"
	roguelikeWhatStartRun        = "BlackFlowStartRun"
)

const (
	roguelikeStartRunCompleted         = "completed"
	roguelikeStartRunRewardPageMissing = "reward_page_missing"
	roguelikeStartRunRecognitionFailed = "reward_recognition_failed"
	roguelikeStartRunUnrecognized      = "reward_unrecognized"
)

const roguelikeMaxSubtypeBytes = 256

func roguelikeRunTargetID(run int) string { return "roguelike:run:" + strconv.Itoa(run) }

func roguelikeInvestmentCanStop(cfg roguelikeConfig) bool {
	if !cfg.InvestmentEnabled || cfg.InvestmentsCount == 0 {
		return false
	}
	return cfg.InvestmentsCount >= 1 || cfg.StopWhenInvestmentFull
}

func roguelikeObjective(cfg roguelikeConfig) (control.ObjectivePolicy, []string, control.TaskReducer) {
	switch cfg.Mode {
	case roguelikeModeExp:
		if cfg.Theme == roguelikeThemeBlackFlow {
			return control.ObjectiveNone, nil, nil
		}
		if cfg.StartsCount < 1 || cfg.StartsCount > control.MaxTypedTargetsPerTask ||
			cfg.StopAtFinalBoss || cfg.StopAtMaxLevel || roguelikeInvestmentCanStop(cfg) {
			return control.ObjectiveNone, nil, nil
		}
		targets := make([]string, cfg.StartsCount)
		for i := range targets {
			targets[i] = roguelikeRunTargetID(i + 1)
		}
		policy := control.ObjectiveAny
		if cfg.StartsCount == 1 {
			policy = control.ObjectiveAll
		}
		return policy, targets, &roguelikeReducer{mode: cfg.Mode, runs: cfg.StartsCount}
	case roguelikeModeInvestment:
		if cfg.Theme == roguelikeThemeBlackFlow || !roguelikeInvestmentCanStop(cfg) {
			return control.ObjectiveNone, nil, nil
		}
		return roguelikeSingleObjective(cfg.Mode, roguelikeTargetInvestment)
	case roguelikeModeSquad:
		if !cfg.MonthlySquadAutoIterate {
			return control.ObjectiveNone, nil, nil
		}
		return roguelikeSingleObjective(cfg.Mode, roguelikeTargetMonthlySquad)
	case roguelikeModeExploration:
		if !cfg.DeepExplorationAutoIterate {
			return control.ObjectiveNone, nil, nil
		}
		return roguelikeSingleObjective(cfg.Mode, roguelikeTargetDeepExploration)
	case roguelikeModeFindPlaytime:
		return roguelikeSingleObjective(cfg.Mode, roguelikeTargetJieGarden)
	case roguelikeModeBlackFlowStartRun:
		return roguelikeSingleObjective(cfg.Mode, roguelikeTargetStartRun)
	default:
		return control.ObjectiveNone, nil, nil
	}
}

func roguelikeSingleObjective(mode int, target string) (control.ObjectivePolicy, []string, control.TaskReducer) {
	return control.ObjectiveAll, []string{target}, &roguelikeReducer{mode: mode, target: target}
}

type roguelikeReducer struct {
	mode    int
	target  string
	runs    int
	coreID  int32
	runSeqs map[uint64]bool
}

func roguelikeCallbackFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: %s", control.ErrTypedResult, "roguelike: callback must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := strictjson.CheckKeys(dec); err != nil {
		return nil, fmt.Errorf("%w: %s", control.ErrTypedResult, "roguelike: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return nil, fmt.Errorf("%w: %s", control.ErrTypedResult, "roguelike: callback must be a JSON object")
	}
	return top, nil
}

func (r *roguelikeReducer) Observe(o control.Observation) (control.TypedUpdate, error) {
	if o.MessageID != roguelikeMessageSubTaskStart && o.MessageID != roguelikeMessageSubTaskExtraInfo || len(o.JSON) == 0 {
		return control.TypedUpdate{}, nil
	}
	top, err := roguelikeCallbackFields(o.JSON)
	if err != nil {
		return control.TypedUpdate{}, err
	}
	if chain, ok := strictjson.FieldString(top, "taskchain"); !ok || chain != roguelikeTaskType {
		return control.TypedUpdate{}, nil
	}
	var coreID int32
	if json.Unmarshal(top["taskid"], &coreID) != nil || coreID <= 0 {
		return control.TypedUpdate{}, nil
	}
	if r.coreID == 0 {
		r.coreID = coreID
	} else if r.coreID != coreID {
		return control.TypedUpdate{}, nil
	}
	if o.MessageID == roguelikeMessageSubTaskStart {
		return r.observeNodeStart(o, top), nil
	}
	return r.observeExtraInfo(top), nil
}

func (r *roguelikeReducer) observeNodeStart(o control.Observation, top map[string]json.RawMessage) control.TypedUpdate {
	if r.runs == 0 {
		return control.TypedUpdate{}
	}
	if subtask, ok := strictjson.FieldString(top, "subtask"); !ok || subtask != roguelikeSubtaskProcessTask {
		return control.TypedUpdate{}
	}
	fields, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return control.TypedUpdate{}
	}
	node, _ := strictjson.FieldString(fields, "task")
	var outcome control.TargetOutcome
	var code string
	switch node {
	case roguelikeNodeGamePass:
		outcome, code = control.TargetAchieved, roguelikeCodeGamePass
	case roguelikeNodeMissionFailedFlag2:
		outcome, code = control.TargetFailed, roguelikeCodeMissionFailed
	default:
		return control.TypedUpdate{}
	}
	if r.runSeqs[o.CoreSequence] {
		return control.TypedUpdate{}
	}
	if len(r.runSeqs) >= r.runs {
		return control.TypedUpdate{}
	}
	if r.runSeqs == nil {
		r.runSeqs = make(map[uint64]bool, r.runs)
	}
	r.runSeqs[o.CoreSequence] = true
	return control.TypedUpdate{Targets: []control.TargetResult{{
		ID:      roguelikeRunTargetID(len(r.runSeqs)),
		Outcome: outcome,
		Code:    code,
	}}}
}

func (r *roguelikeReducer) observeExtraInfo(top map[string]json.RawMessage) control.TypedUpdate {
	if r.target == "" {
		return control.TypedUpdate{}
	}
	what, ok := strictjson.FieldString(top, "what")
	if !ok {
		return control.TypedUpdate{}
	}
	if _, ok := strictjson.ObjectField(top, "details"); !ok {
		return control.TypedUpdate{}
	}
	switch r.mode {
	case roguelikeModeInvestment:
		switch what {
		case roguelikeWhatInvestmentLimit:
			return r.singleFact(control.TargetAchieved, roguelikeCodeInvestmentLimit, nil)
		case roguelikeWhatInvestmentFull:
			return r.singleFact(control.TargetAchieved, roguelikeCodeInvestmentFull, nil)
		}
	case roguelikeModeSquad:
		if what == roguelikeWhatMonthlySquad {
			return r.singleFact(control.TargetAchieved, roguelikeCodeRotationDone, nil)
		}
	case roguelikeModeExploration:
		if what == roguelikeWhatDeepExploration {
			return r.singleFact(control.TargetAchieved, roguelikeCodeRotationDone, nil)
		}
	case roguelikeModeFindPlaytime:
		if what == roguelikeWhatJieGardenFound {
			return r.singleFact(control.TargetAchieved, roguelikeCodeTargetFound, roguelikeSubtypeDetails(top))
		}
	case roguelikeModeBlackFlowStartRun:
		if what == roguelikeWhatStartRun {
			return r.startRunFact(top)
		}
	}
	return control.TypedUpdate{}
}

func (r *roguelikeReducer) startRunFact(top map[string]json.RawMessage) control.TypedUpdate {
	fields, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return control.TypedUpdate{}
	}
	status, ok := strictjson.FieldString(fields, "status")
	if !ok {
		return control.TypedUpdate{}
	}
	switch status {
	case roguelikeStartRunCompleted:
		return r.singleFact(control.TargetAchieved, roguelikeCodeStartRunDone, nil)
	case roguelikeStartRunRewardPageMissing, roguelikeStartRunRecognitionFailed, roguelikeStartRunUnrecognized:
		return r.singleFact(control.TargetFailed, status, nil)
	default:
		return control.TypedUpdate{}
	}
}

func (r *roguelikeReducer) singleFact(outcome control.TargetOutcome, code string, details json.RawMessage) control.TypedUpdate {
	return control.TypedUpdate{Targets: []control.TargetResult{{
		ID:      r.target,
		Outcome: outcome,
		Code:    code,
		Details: details,
	}}}
}

func roguelikeSubtypeDetails(top map[string]json.RawMessage) json.RawMessage {
	fields, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return nil
	}
	subtype, ok := strictjson.FieldString(fields, "target_subtype")
	if !ok || !strictjson.ValidText(subtype, roguelikeMaxSubtypeBytes, true) {
		return nil
	}
	encoded, err := json.Marshal(struct {
		Subtype string `json:"target_subtype"`
	}{subtype})
	if err != nil {
		return nil
	}
	return encoded
}

func (r *roguelikeReducer) IsUnassociatedProgress(o control.Observation) bool {
	if o.MessageID < 20000 || o.MessageID > 20004 || len(o.JSON) == 0 {
		return false
	}
	top, err := roguelikeCallbackFields(o.JSON)
	if err != nil {
		return false
	}
	if chain, ok := strictjson.FieldString(top, "taskchain"); !ok || chain != roguelikeTaskType {
		return false
	}
	if !bytes.Equal(bytes.TrimSpace(top["taskid"]), []byte("0")) {
		return false
	}
	return true
}

var _ control.UnassociatedObservationClassifier = (*roguelikeReducer)(nil)
