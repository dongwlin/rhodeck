package roguelike

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

// Core callback message kinds. SubTaskStart (20001) carries the settlement
// node names; SubTaskExtraInfo (20003) carries the what-based facts. All
// Roguelike tasks, BlackFlow included, run on one task chain (RoguelikeTask.h
// TaskType).
const (
	roguelikeMessageSubTaskStart     int32 = 20001
	roguelikeMessageSubTaskExtraInfo int32 = 20003
	roguelikeSubtaskProcessTask            = "ProcessTask"
)

// Stable target IDs. Mode 0 names one target per declared run; every other
// interpreted mode names its single mode objective.
const (
	roguelikeTargetInvestment      = "roguelike:investment"
	roguelikeTargetMonthlySquad    = "roguelike:monthly_squad"
	roguelikeTargetDeepExploration = "roguelike:deep_exploration"
	roguelikeTargetJieGarden       = "roguelike:jie_garden_target"
	roguelikeTargetStartRun        = "roguelike:start_run"
)

// Stable outcome codes derived from exact Core node names, what values and
// status values.
const (
	roguelikeCodeGamePass        = "game_pass"
	roguelikeCodeMissionFailed   = "mission_failed"
	roguelikeCodeInvestmentLimit = "investment_limit_reached"
	roguelikeCodeInvestmentFull  = "investment_full"
	roguelikeCodeRotationDone    = "rotation_completed"
	roguelikeCodeTargetFound     = "target_found"
	roguelikeCodeStartRunDone    = "start_run_completed"
)

// Settlement node names exactly as Core strips them from details.task
// (AbstractTask removes the last "@..." prefix before emitting).
const (
	roguelikeNodeGamePass           = "GamePass"
	roguelikeNodeMissionFailedFlag2 = "MissionFailedFlag2"
)

// Extra-info what values that establish a business fact. Every other what,
// including RoguelikeSettlement, RoguelikeInvestment progress and
// BlackFlowStrategyResult, is progress or diagnostics and stays uninterpreted.
const (
	roguelikeWhatInvestmentLimit = "RoguelikeInvestmentReachLimit"
	roguelikeWhatInvestmentFull  = "RoguelikeInvestmentReachFull"
	roguelikeWhatMonthlySquad    = "MonthlySquadCompleted"
	roguelikeWhatDeepExploration = "DeepExplorationCompleted"
	roguelikeWhatJieGardenFound  = "RoguelikeJieGardenTargetFound"
	roguelikeWhatStartRun        = "BlackFlowStartRun"
)

// BlackFlowStartRun details.status values. completed proves the objective and
// the three reward_* values are chain-terminal failures that stop the chain.
// preparing, target_run and reward_missed are strategy-loop progress: Core
// abandons the run and continues with the next preparation, so none of them is
// a task-terminal fact and none may preempt a later completion of the target.
const (
	roguelikeStartRunCompleted         = "completed"
	roguelikeStartRunRewardPageMissing = "reward_page_missing"
	roguelikeStartRunRecognitionFailed = "reward_recognition_failed"
	roguelikeStartRunUnrecognized      = "reward_unrecognized"
)

// roguelikeMaxSubtypeBytes bounds the optional JieGarden target subtype that
// is copied into public target details.
const roguelikeMaxSubtypeBytes = 256

// roguelikeRunTargetID names the k-th prepared run, one-based.
func roguelikeRunTargetID(run int) string { return "roguelike:run:" + strconv.Itoa(run) }

// roguelikeInvestmentCanStop reports whether Core can emit
// RoguelikeInvestmentReachLimit or RoguelikeInvestmentReachFull at all. Both
// facts end the chain via stop_roguelike, and both are emitted only from the
// plugin's run, which the StageTraderInvestConfirm verify gates on
// m_invest_count < m_maximum: an explicit investment_enabled=false disables
// the StageTraderInvestSystem node itself, a zero investments_count makes that
// verify always false, Core's INT_MAX default keeps the accumulated count
// below the limit, and the full callback additionally requires
// stop_when_investment_full=true. Where neither callback can fire, the
// investment objective is unprovable and the experience mode's declared runs
// cannot be cut short by the investment system.
func roguelikeInvestmentCanStop(cfg roguelikeConfig) bool {
	if !cfg.InvestmentEnabled || cfg.InvestmentsCount == 0 {
		return false
	}
	return cfg.InvestmentsCount >= 1 || cfg.StopWhenInvestmentFull
}

// roguelikeObjective selects the objective policy, targets and reducer from
// the compiled input. It is pure: the same configuration always yields the
// same preparation, and configurations without a reachable business fact stay
// input-only instead of declaring targets that could never leave unobserved.
func roguelikeObjective(cfg roguelikeConfig) (control.ObjectivePolicy, []string, control.TaskReducer) {
	switch cfg.Mode {
	case roguelikeModeExp:
		// The BlackFlow theme runs a burn-style strategy profile whose runs are
		// collected per floor and restarted before any settlement screen, so
		// GamePass and MissionFailedFlag2 are not its run endings. Every other
		// theme needs an explicit bounded run count and no configuration that
		// can end the chain before the declared runs settle: either early-stop
		// switch stops after the current run, and a reachable investment limit
		// or full callback stops mid-runs the same way, which would leave the
		// remaining runs unprovable.
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
		// The BlackFlow theme runs its own investment strategy profile instead
		// of these callbacks; every other configuration without a reachable
		// limit or full callback stays input-only instead of declaring a target
		// that could never leave unobserved.
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
		// Modes 4, 5, 10001 and 30001 have no stable terminal callback.
		return control.ObjectiveNone, nil, nil
	}
}

// roguelikeSingleObjective prepares the one-target interpretations.
func roguelikeSingleObjective(mode int, target string) (control.ObjectivePolicy, []string, control.TaskReducer) {
	return control.ObjectiveAll, []string{target}, &roguelikeReducer{mode: mode, target: target}
}

// roguelikeReducer interprets callbacks for one prepared roguelike task. The
// runtime observes it serially on its single monitor goroutine, so it needs no
// internal locking. Scratch state is bounded by the prepared target count and
// never retains Observation.JSON.
type roguelikeReducer struct {
	mode int
	// target is the single objective of the non-experience modes; mode 0 uses
	// per-run targets instead.
	target string
	// runs is the declared run count of mode 0.
	runs int
	// coreID fixes the accepted Core task id after the first associated
	// callback. Only that id's callbacks contribute facts.
	coreID int32
	// runSeqs records the binding sequence of every counted settlement so a
	// re-delivered callback cannot shift a later run onto the wrong target.
	runSeqs map[uint64]bool
}

// roguelikeCallbackFields decodes one callback object. Duplicate keys at any
// depth would make every consumed field ambiguous, so they are rejected before
// any field is interpreted.
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

// Observe interprets one Core callback. Unrelated message kinds, foreign task
// chains and foreign task ids are ignored so the targets stay unobserved
// instead of gaining a fabricated fact. Chain notifications such as
// TaskChainCompleted establish no business fact. Only an unusable callback
// object (malformed JSON or duplicate keys at any depth) returns an error,
// which the runtime maps to typed_invalid_event without admitting an update.
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
		// Only the accepted submission's callbacks contribute; a different
		// task id is never re-associated.
		return control.TypedUpdate{}, nil
	}
	if o.MessageID == roguelikeMessageSubTaskStart {
		return r.observeNodeStart(o, top), nil
	}
	return r.observeExtraInfo(top), nil
}

// observeNodeStart counts mode 0 run settlements. Core emits exactly one
// GamePass or MissionFailedFlag2 SubTaskStart per completed run and both nodes
// continue into the next run, so FIFO arrival order pairs the k-th settlement
// with the k-th run without timestamps. Settlements beyond the declared run
// count cannot name a prepared target and are ignored.
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

// observeExtraInfo interprets the extra-info facts of the single-target modes.
// An extra-info event needs a what and a details object; a missing or unusable
// one leaves the target unobserved instead of inventing a fact.
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

// startRunFact reads the start-run lifecycle status. completed proves the
// objective; the three chain-terminal reward statuses prove the attempt failed
// and Core stopped the chain. preparing, target_run, reward_missed and any
// unknown status establish no target fact and never preempt a later
// completion of the same target.
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

// singleFact returns the one prepared target's fact. Repeating the identical
// fact is harmless: the shared typed update treats byte-equal facts as
// idempotent, and a different fact for the recorded target is a conflict the
// runtime rejects while preserving the original.
func (r *roguelikeReducer) singleFact(outcome control.TargetOutcome, code string, details json.RawMessage) control.TypedUpdate {
	return control.TypedUpdate{Targets: []control.TargetResult{{
		ID:      r.target,
		Outcome: outcome,
		Code:    code,
		Details: details,
	}}}
}

// roguelikeSubtypeDetails exposes only the optional JieGarden target subtype.
// The found fact comes from the what itself, so a missing, non-string or
// invalid subtype cannot unprove it and is simply not published.
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
		// A NUL-free valid UTF-8 string cannot fail to marshal; the details
		// stay omitted rather than publishing an unexpected encoding.
		return nil
	}
	return encoded
}

// IsUnassociatedProgress classifies the BlackFlow port's internal callbacks. The
// BlackFlow theme drives map routing and inventory cleanup through an internal
// AbstractTask that never receives a task id, so its subtask callbacks carry
// taskid 0 on the same Roguelike chain while an adapted task is being
// observed. They cannot belong to any accepted submission and establish no
// business fact; recognizing exactly that shape keeps them from being misread
// as lost execution associations while every other unassociated event stays on
// the runtime's strict path.
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
