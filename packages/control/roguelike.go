package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	roguelikeTaskType    = "Roguelike"
	roguelikeAdapterName = "roguelike"
	roguelikeVersion     = 1
)

// Theme names are exact, case-sensitive Core identifiers.
const (
	roguelikeThemePhantom   = "Phantom"
	roguelikeThemeMizuki    = "Mizuki"
	roguelikeThemeSami      = "Sami"
	roguelikeThemeSarkaz    = "Sarkaz"
	roguelikeThemeJieGarden = "JieGarden"
	roguelikeThemeBlackFlow = "BlackFlow"
)

// Mode values follow the frozen Core whitelist. Modes 2 and 3 are never valid.
const (
	roguelikeModeExp                 = 0
	roguelikeModeInvestment          = 1
	roguelikeModeCollapsalParadigm   = 5     // Sami only
	roguelikeModeCollectible         = 4     // every theme except BlackFlow
	roguelikeModeSquad               = 6     // every theme except BlackFlow
	roguelikeModeExploration         = 7     // every theme except BlackFlow
	roguelikeModeFastPass            = 10001 // Sarkaz only
	roguelikeModeFindPlaytime        = 20001 // JieGarden only
	roguelikeModeBlackFlowBabyAnimal = 30001 // BlackFlow only
	roguelikeModeBlackFlowStartRun   = 30003 // BlackFlow only
)

var roguelikeThemes = []string{
	roguelikeThemePhantom, roguelikeThemeMizuki, roguelikeThemeSami,
	roguelikeThemeSarkaz, roguelikeThemeJieGarden, roguelikeThemeBlackFlow,
}

// Frozen Core value sets for BlackFlow string fields. An unknown value would
// only disable the Core lifecycle plugin, which is not an observable failure,
// so the adapter rejects it instead.
var (
	roguelikeBlackFlowStrategies = []string{
		"baby_animal", "baby_animal_floor3", "burn", "burn_with_investment",
		"ending1", "ending2", "ending3", "investment",
		"start_run_animals", "start_run_rewards", "start_run_rewards_with_investment",
	}
	roguelikeBlackFlowAnimals = []string{
		"swaddled_cat", "swaddled_dog", "swaddled_cerberus", "swaddled_feathered_serpent",
	}
	roguelikeBlackFlowDiagnostics = []string{"normal", "detailed", "full"}
	// Reward ids in bit order of blackflow_start_rewards.
	roguelikeBlackFlowRewardIDs = []string{
		"unnumbered_item", "survey_advance", "space_lease", "regression_compensation",
		"forest_ride", "nest_parasitism", "pegasus", "hippogriff",
		"dragon", "hydra", "bai_ze", "golden_crow",
	}
)

// NewRoguelikeAdapter returns the strict, explicitly selected roguelike@1
// adapter for the Roguelike Core task. Raw Roguelike tasks remain open Core
// inputs.
//
// Prepare requires an explicit theme and mode. theme is exactly one of
// Phantom, Mizuki, Sami, Sarkaz, JieGarden or BlackFlow. mode follows the
// frozen per-theme matrix: 0, 1, 4, 6 and 7 for every theme, plus 5 for Sami,
// 10001 for Sarkaz and 20001 for JieGarden; BlackFlow only accepts 0, 1, 30001
// and 30003. Modes 2 and 3 and every other theme/mode combination are
// rejected.
//
// Common fields are difficulty (-1, an integer 0-20 or 2147483647), squad,
// roles, core_char, core_char_list, use_support, use_nonfriend_support,
// starts_count, stop_at_final_boss, stop_at_max_level, investment_enabled,
// investments_count, investment_with_more_score and stop_when_investment_full.
// starts_count and investments_count reject negative values; other integers
// without a frozen range keep their literal value. Theme and mode fields are
// accepted only for their own theme or mode: refresh_trader_with_dice for
// Mizuki; use_foldartal, start_foldartal_list, check_collapsal_paradigms,
// double_check_collapsal_paradigms, expected_collapsal_paradigms and
// first_floor_foldartal (a string, not a boolean) for Sami; start_with_seed (a
// nonempty string) for Sarkaz; collectible_mode_shopping and
// collectible_mode_start_list for mode 4, whose boolean sub-keys hot_water,
// shield, ingot, hope and random are shared and key, dice, ideas and ticket
// are Mizuki, Mizuki, Sarkaz and JieGarden only; collectible_mode_squad for
// mode 4 and the BlackFlow start-run mode; monthly_squad_auto_iterate and
// monthly_squad_check_comms for mode 6; deep_exploration_auto_iterate for mode
// 7; find_playTime_target (1-3) for mode 20001; and the blackflow_* fields,
// collectible_mode_roles and collectible_mode_core_char_list for BlackFlow.
//
// Combination constraints: start_with_elite_two and only_start_with_elite_two
// are mode-4 only, and only_start_with_elite_two requires
// start_with_elite_two=true. Mode 20001 requires find_playTime_target. Mode
// 30003 requires blackflow_start_animals (0-2 known animal ids, at most one
// swaddled_feathered_serpent) and blackflow_start_rewards (0-4095); reward
// bits above 0x3F require swaddled_feathered_serpent, which Core would
// otherwise truncate away silently. Mode 30003 rejects an explicit
// blackflow_strategy, which Core would override silently. Mode 30001 requires
// an explicit blackflow_strategy. blackflow_start_utopia=true requires a
// nonempty blackflow_start_reward_priority of distinct known reward ids.
// blackflow_strategy, blackflow_cultivation_target, blackflow_diagnostics and
// blackflow_diagnostic_image_limit are validated against their frozen value
// sets instead of letting Core disable its lifecycle plugin silently.
//
// Unknown fields at any depth, explicit nulls, duplicate keys at any depth,
// type errors, non-object roots, empty or all-whitespace strings, invalid
// UTF-8, unpaired UTF-16 surrogate escapes, NUL, misplaced theme or mode
// fields, investment_enter_second_floor, out-of-range values and input or
// compiled JSON over 64 KiB are rejected with an error wrapping ErrInvalidPlan.
// Strings are kept exactly as submitted: never trimmed, normalized or
// path-cleaned.
//
// Compilation keeps only the fields and values the input actually provides; no
// Core default is injected and number literals never round-trip through
// float64. The compiled theme, mode and objective-relevant switches select the
// result interpretation. Mode 0 declares one target per run,
// roguelike:run:1..N from an explicit starts_count of 1-128, with policy any
// for several runs and all for a single run, and only while nothing can end
// the chain before the declared runs settle: stop_at_final_boss or
// stop_at_max_level stops after the current run, and a reachable investment
// limit or full callback (an explicit positive investments_count, or
// stop_when_investment_full=true with a nonzero count, while investment
// stays enabled) stops mid-runs the same way. The BlackFlow theme collects its
// strategy runs per floor before any settlement screen. Those configurations
// stay input-only. Mode 1 declares roguelike:investment with policy all only
// when the limit or full callback is reachable at all: an explicit
// investment_enabled=false disables Core's investment system, a zero
// investments_count keeps its investment plugin from engaging, Core's defaults
// (INT_MAX and false) leave the limit unreachable, and the BlackFlow theme
// runs its own investment strategy profile instead, so those configurations
// stay input-only. Mode 6 and mode 7 declare
// roguelike:monthly_squad and roguelike:deep_exploration with policy all only
// when monthly_squad_auto_iterate or deep_exploration_auto_iterate is true,
// since Core never emits the rotation callbacks otherwise. Mode 20001 declares
// roguelike:jie_garden_target and mode 30003 declares roguelike:start_run,
// both with policy all. Every other combination stays input-only with
// ObjectiveNone, no targets and no reducer.
//
// Facts come only from callbacks of the accepted Core task with
// taskchain=Roguelike. The settlement nodes GamePass (achieved, game_pass) and
// MissionFailedFlag2 (failed, mission_failed) count runs in arrival order, one
// target per run; settlements beyond the declared run count are ignored. The
// extra-info facts RoguelikeInvestmentReachLimit (achieved,
// investment_limit_reached), RoguelikeInvestmentReachFull (achieved,
// investment_full), MonthlySquadCompleted and DeepExplorationCompleted
// (achieved, rotation_completed), RoguelikeJieGardenTargetFound (achieved,
// target_found, exposing only an optional bounded target_subtype) and
// BlackFlowStartRun status completed (achieved, start_run_completed) or a
// chain-terminal reward_page_missing, reward_recognition_failed or
// reward_unrecognized (failed, that status) are interpreted exactly. Chain
// completion, stops, running statuses, reward_missed and unknown statuses
// never create facts, and missing evidence stays unobserved. The BlackFlow
// port's internal callbacks (taskid 0) are environmental and create no facts.
//
// Preparation is deterministic and side-effect free: it creates no directory,
// takes no lock, launches no worker, loads no native code and connects no
// device. It verifies local structure and budgets only; theme unlock state,
// squad and operator names, seeds, foldartal names and reward identifiers
// against the installed resources, and device readiness remain unverified.
func NewRoguelikeAdapter() TaskAdapter { return roguelikeAdapter{} }

type roguelikeAdapter struct{}

func (roguelikeAdapter) Descriptor() AdapterRef {
	return AdapterRef{Name: roguelikeAdapterName, Version: roguelikeVersion}
}

func (roguelikeAdapter) Prepare(task Task) (PreparedTask, error) {
	if task.Type != roguelikeTaskType {
		return PreparedTask{}, roguelikeReject("adapter requires Core type %q", roguelikeTaskType)
	}
	params, cfg, err := compileRoguelike(task.Params)
	if err != nil {
		return PreparedTask{}, err
	}
	policy, targets, reducer := roguelikeObjective(cfg)
	if policy == ObjectiveNone {
		return PreparedTask{Params: params, Policy: ObjectiveNone}, nil
	}
	return PreparedTask{Params: params, Policy: policy, Targets: targets, Reducer: reducer}, nil
}

// roguelikeReject wraps every input rejection with ErrInvalidPlan so callers
// classify failures with errors.Is instead of inspecting diagnostics.
func roguelikeReject(format string, args ...any) error {
	return fmt.Errorf("%w: roguelike: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

// roguelikeConfig is the compiled input structure this adapter keeps in
// package scope for objective interpretation. Effective values reflect the
// Core defaults of omitted switches so interpretation can rely on them; they
// are never injected into the compiled Core params.
type roguelikeConfig struct {
	Theme string
	Mode  int
	// MonthlySquadAutoIterate and DeepExplorationAutoIterate select whether the
	// corresponding rotation callbacks can appear at all (Core default false).
	MonthlySquadAutoIterate    bool
	DeepExplorationAutoIterate bool
	// FindPlayTimeTarget is 1-3 for the JieGarden playtime mode and 0 otherwise.
	FindPlayTimeTarget int
	// StartsCount and InvestmentsCount are -1 when the field is absent,
	// otherwise the validated non-negative value. StopAtFinalBoss,
	// StopAtMaxLevel and StopWhenInvestmentFull keep the submitted switch
	// state with Core's false default. The experience, investment and
	// early-stop values select the mode 0 and mode 1 interpretations.
	StartsCount            int
	InvestmentsCount       int
	StopAtFinalBoss        bool
	StopAtMaxLevel         bool
	StopWhenInvestmentFull bool
	// BlackFlow profile switches. Core derives the start-run profile from the
	// animal count and the investment flag, so both keep their effective
	// values: InvestmentEnabled defaults to true and BlackFlowStartAnimals is
	// -1 when the field is absent, otherwise the validated 0-2 entry count.
	// BlackFlowStartSerpent records whether that list carries the feathered
	// serpent, which gates the start-run reward bits above 0x3F.
	InvestmentEnabled     bool
	BlackFlowStartAnimals int
	BlackFlowStartSerpent bool
	BlackFlowUtopia       bool
}

// compileRoguelike validates the strict roguelike input and returns the final
// Core params plus the compiled configuration. It is deterministic and has no
// file, native or device effects.
func compileRoguelike(raw json.RawMessage) (json.RawMessage, roguelikeConfig, error) {
	if !validObject(raw, MaxAdapterParamsBytes) {
		return nil, roguelikeConfig{}, roguelikeReject("params must be a JSON object of at most %d bytes", MaxAdapterParamsBytes)
	}
	// Duplicate keys at any depth are rejected before any field is interpreted.
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := checkTypedKeys(keys); err != nil {
		return nil, roguelikeConfig{}, roguelikeReject("params contain duplicate or malformed fields")
	}
	// RawMessage values keep number literals exact and string bytes verbatim;
	// map keys are exact too, so case variants such as Theme stay distinct.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, roguelikeConfig{}, roguelikeReject("params must be a JSON object")
	}
	for name, value := range fields {
		if !roguelikeKnownField(name) {
			return nil, roguelikeConfig{}, roguelikeReject("unknown params field %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, roguelikeConfig{}, roguelikeReject("%s must not be null", name)
		}
	}

	// theme and mode must be explicit; Core defaults never apply here.
	themeRaw, ok := fields["theme"]
	if !ok {
		return nil, roguelikeConfig{}, roguelikeReject("theme is required")
	}
	theme, err := operProgressDecodeString(themeRaw)
	if err != nil || !roguelikeValidTheme(theme) {
		return nil, roguelikeConfig{}, roguelikeReject("theme must be one of %s", strings.Join(roguelikeThemes, ", "))
	}
	modeRaw, ok := fields["mode"]
	if !ok {
		return nil, roguelikeConfig{}, roguelikeReject("mode is required")
	}
	mode, err := operProgressInteger(modeRaw)
	if err != nil {
		return nil, roguelikeConfig{}, roguelikeReject("mode must be an integer")
	}
	if !roguelikeValidMode(theme, mode) {
		return nil, roguelikeConfig{}, roguelikeReject("mode %d is not valid for theme %q", mode, theme)
	}

	cfg := roguelikeConfig{
		Theme:                 theme,
		Mode:                  mode,
		InvestmentEnabled:     true, // Core default
		StartsCount:           -1,
		InvestmentsCount:      -1,
		BlackFlowStartAnimals: -1,
	}
	// startRewards keeps the validated blackflow_start_rewards value, or -1
	// when the field is absent, for the start-run reward bit rule.
	startRewards := -1
	for name, value := range fields {
		if !roguelikeAllowsField(name, theme, mode) {
			return nil, roguelikeConfig{}, roguelikeReject("field %q is not allowed for theme %q mode %d", name, theme, mode)
		}
		switch name {
		case "theme", "mode":
			// Already validated and captured before this loop.
		case "use_support", "use_nonfriend_support", "stop_at_final_boss", "stop_at_max_level",
			"investment_enabled", "investment_with_more_score", "stop_when_investment_full",
			"start_with_elite_two", "only_start_with_elite_two", "collectible_mode_shopping",
			"refresh_trader_with_dice", "monthly_squad_auto_iterate", "monthly_squad_check_comms",
			"deep_exploration_auto_iterate", "use_foldartal", "check_collapsal_paradigms",
			"double_check_collapsal_paradigms", "blackflow_start_utopia":
			flag, err := roguelikeBoolField(value, name)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			switch name {
			case "investment_enabled":
				cfg.InvestmentEnabled = flag
			case "monthly_squad_auto_iterate":
				cfg.MonthlySquadAutoIterate = flag
			case "deep_exploration_auto_iterate":
				cfg.DeepExplorationAutoIterate = flag
			case "blackflow_start_utopia":
				cfg.BlackFlowUtopia = flag
			case "stop_at_final_boss":
				cfg.StopAtFinalBoss = flag
			case "stop_at_max_level":
				cfg.StopAtMaxLevel = flag
			case "stop_when_investment_full":
				cfg.StopWhenInvestmentFull = flag
			}
		case "difficulty":
			n, err := roguelikeIntField(value, name)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			// Frozen from Core difficulty selection: keep the current value,
			// the OCR enumeration range 0-20, or the pull-to-maximum sentinel.
			if n != -1 && n != 2147483647 && (n < 0 || n > 20) {
				return nil, roguelikeConfig{}, roguelikeReject("difficulty must be -1, an integer 0-20 or 2147483647")
			}
		case "starts_count", "investments_count":
			n, err := roguelikeIntField(value, name)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			if n < 0 {
				return nil, roguelikeConfig{}, roguelikeReject("%s must not be negative", name)
			}
			if name == "starts_count" {
				cfg.StartsCount = n
			} else {
				cfg.InvestmentsCount = n
			}
		case "squad", "roles", "core_char", "collectible_mode_squad", "collectible_mode_roles",
			"first_floor_foldartal", "start_with_seed":
			if _, err := roguelikeStringField(value, name, MaxAdapterParamsBytes); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "blackflow_strategy":
			if _, err := roguelikeEnumField(value, name, roguelikeBlackFlowStrategies); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "blackflow_cultivation_target":
			if _, err := roguelikeEnumField(value, name, roguelikeBlackFlowAnimals); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "blackflow_diagnostics":
			if _, err := roguelikeEnumField(value, name, roguelikeBlackFlowDiagnostics); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "blackflow_diagnostic_image_limit":
			n, err := roguelikeIntField(value, name)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			if n < 0 || n > 100 {
				return nil, roguelikeConfig{}, roguelikeReject("blackflow_diagnostic_image_limit must be an integer 0-100")
			}
		case "blackflow_start_rewards":
			n, err := roguelikeIntField(value, name)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			if n < 0 || n > 0xFFF {
				return nil, roguelikeConfig{}, roguelikeReject("blackflow_start_rewards must be an integer 0-4095")
			}
			startRewards = n
		case "find_playTime_target":
			n, err := roguelikeIntField(value, name)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			if n < 1 || n > 3 {
				return nil, roguelikeConfig{}, roguelikeReject("find_playTime_target must be an integer 1-3")
			}
			cfg.FindPlayTimeTarget = n
		case "core_char_list", "collectible_mode_core_char_list":
			if err := roguelikeOperListField(value, name); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "start_foldartal_list":
			if err := roguelikeFoldartalListField(value); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "expected_collapsal_paradigms":
			if err := roguelikeParadigmListField(value); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "blackflow_start_animals":
			count, serpent, err := roguelikeBlackFlowAnimalsField(value)
			if err != nil {
				return nil, roguelikeConfig{}, err
			}
			cfg.BlackFlowStartAnimals = count
			cfg.BlackFlowStartSerpent = serpent
		case "blackflow_start_reward_priority":
			if err := roguelikeBlackFlowRewardPriorityField(value); err != nil {
				return nil, roguelikeConfig{}, err
			}
		case "collectible_mode_start_list":
			if err := roguelikeCollectibleStartListField(value, theme); err != nil {
				return nil, roguelikeConfig{}, err
			}
		default:
			return nil, roguelikeConfig{}, roguelikeReject("unknown params field %q", name)
		}
	}

	// Combination constraints that span several fields.
	if _, ok := fields["only_start_with_elite_two"]; ok {
		start, hasStart := fields["start_with_elite_two"]
		if !hasStart || string(bytes.TrimSpace(start)) != "true" {
			return nil, roguelikeConfig{}, roguelikeReject("only_start_with_elite_two requires start_with_elite_two=true")
		}
	}
	if mode == roguelikeModeFindPlaytime {
		if _, ok := fields["find_playTime_target"]; !ok {
			return nil, roguelikeConfig{}, roguelikeReject("mode 20001 requires find_playTime_target")
		}
	}
	if mode == roguelikeModeBlackFlowStartRun {
		if _, ok := fields["blackflow_start_animals"]; !ok {
			return nil, roguelikeConfig{}, roguelikeReject("mode 30003 requires blackflow_start_animals")
		}
		if _, ok := fields["blackflow_start_rewards"]; !ok {
			return nil, roguelikeConfig{}, roguelikeReject("mode 30003 requires blackflow_start_rewards")
		}
		// Core silently truncates reward bits above 0x3F when the animal list
		// carries no feathered serpent; rejecting the divergence keeps the run
		// from differing from the submitted input.
		if !cfg.BlackFlowStartSerpent && (startRewards&^0x3F) != 0 {
			return nil, roguelikeConfig{}, roguelikeReject("mode 30003 requires swaddled_feathered_serpent for blackflow_start_rewards bits above 0x3F")
		}
		if _, ok := fields["blackflow_strategy"]; ok {
			// Core overrides any explicit strategy for this mode; accepting it
			// would silently diverge from the submitted input.
			return nil, roguelikeConfig{}, roguelikeReject("mode 30003 does not accept an explicit blackflow_strategy")
		}
	}
	if mode == roguelikeModeBlackFlowBabyAnimal {
		if _, ok := fields["blackflow_strategy"]; !ok {
			return nil, roguelikeConfig{}, roguelikeReject("mode 30001 requires an explicit blackflow_strategy")
		}
	}
	if theme == roguelikeThemeBlackFlow && cfg.BlackFlowUtopia {
		if _, ok := fields["blackflow_start_reward_priority"]; !ok {
			return nil, roguelikeConfig{}, roguelikeReject("blackflow_start_utopia=true requires blackflow_start_reward_priority")
		}
	}

	// json.Marshal sorts map keys, keeps RawMessage values byte-faithful and
	// never routes numbers through float64.
	params, err := json.Marshal(fields)
	if err != nil || len(params) > MaxAdapterParamsBytes {
		return nil, roguelikeConfig{}, roguelikeReject("compiled params exceed %d bytes", MaxAdapterParamsBytes)
	}
	return params, cfg, nil
}

func roguelikeValidTheme(theme string) bool {
	for _, name := range roguelikeThemes {
		if theme == name {
			return true
		}
	}
	return false
}

// roguelikeValidMode mirrors the frozen Core theme/mode whitelist.
func roguelikeValidMode(theme string, mode int) bool {
	switch mode {
	case roguelikeModeExp, roguelikeModeInvestment:
		return true
	case roguelikeModeCollectible, roguelikeModeSquad, roguelikeModeExploration:
		return theme != roguelikeThemeBlackFlow
	case roguelikeModeCollapsalParadigm:
		return theme == roguelikeThemeSami
	case roguelikeModeFastPass:
		return theme == roguelikeThemeSarkaz
	case roguelikeModeFindPlaytime:
		return theme == roguelikeThemeJieGarden
	case roguelikeModeBlackFlowBabyAnimal, roguelikeModeBlackFlowStartRun:
		return theme == roguelikeThemeBlackFlow
	default:
		return false
	}
}

// roguelikeKnownField reports whether name is part of the frozen roguelike
// input schema at all. Scope checks against the selected theme and mode are
// separate so diagnostics can distinguish unknown fields from misplaced ones.
func roguelikeKnownField(name string) bool {
	switch name {
	case "theme", "mode",
		"difficulty", "squad", "roles", "core_char", "core_char_list", "use_support",
		"use_nonfriend_support", "starts_count", "stop_at_final_boss", "stop_at_max_level",
		"investment_enabled", "investments_count", "investment_with_more_score",
		"stop_when_investment_full", "start_with_elite_two", "only_start_with_elite_two",
		"refresh_trader_with_dice",
		"use_foldartal", "start_foldartal_list", "check_collapsal_paradigms",
		"double_check_collapsal_paradigms", "expected_collapsal_paradigms", "first_floor_foldartal",
		"start_with_seed",
		"collectible_mode_shopping", "collectible_mode_squad", "collectible_mode_start_list",
		"monthly_squad_auto_iterate", "monthly_squad_check_comms",
		"deep_exploration_auto_iterate",
		"find_playTime_target",
		"blackflow_strategy", "blackflow_cultivation_target", "blackflow_diagnostics",
		"blackflow_diagnostic_image_limit", "blackflow_start_animals", "blackflow_start_rewards",
		"blackflow_start_utopia", "blackflow_start_reward_priority",
		"collectible_mode_roles", "collectible_mode_core_char_list":
		return true
	default:
		return false
	}
}

// roguelikeAllowsField reports whether the schema field belongs to the
// selected theme and mode. Misplaced fields are rejected instead of being
// silently ignored by Core.
func roguelikeAllowsField(name, theme string, mode int) bool {
	switch name {
	case "theme", "mode",
		"difficulty", "squad", "roles", "core_char", "core_char_list", "use_support",
		"use_nonfriend_support", "starts_count", "stop_at_final_boss", "stop_at_max_level",
		"investment_enabled", "investments_count", "investment_with_more_score",
		"stop_when_investment_full":
		return true
	case "start_with_elite_two", "only_start_with_elite_two",
		"collectible_mode_shopping", "collectible_mode_start_list":
		return mode == roguelikeModeCollectible
	case "collectible_mode_squad":
		// Core consumes this field for the collectible mode and for the
		// BlackFlow start-run preparation squad alike.
		return mode == roguelikeModeCollectible ||
			(theme == roguelikeThemeBlackFlow && mode == roguelikeModeBlackFlowStartRun)
	case "refresh_trader_with_dice":
		return theme == roguelikeThemeMizuki
	case "use_foldartal", "start_foldartal_list", "check_collapsal_paradigms",
		"double_check_collapsal_paradigms", "expected_collapsal_paradigms", "first_floor_foldartal":
		return theme == roguelikeThemeSami
	case "start_with_seed":
		return theme == roguelikeThemeSarkaz
	case "monthly_squad_auto_iterate", "monthly_squad_check_comms":
		return mode == roguelikeModeSquad
	case "deep_exploration_auto_iterate":
		return mode == roguelikeModeExploration
	case "find_playTime_target":
		return mode == roguelikeModeFindPlaytime
	case "blackflow_strategy", "blackflow_cultivation_target", "blackflow_diagnostics",
		"blackflow_diagnostic_image_limit", "blackflow_start_animals", "blackflow_start_rewards",
		"blackflow_start_utopia", "blackflow_start_reward_priority",
		"collectible_mode_roles", "collectible_mode_core_char_list":
		return theme == roguelikeThemeBlackFlow
	default:
		return false
	}
}

// roguelikeBoolField accepts only the JSON literals true and false.
func roguelikeBoolField(raw json.RawMessage, name string) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, roguelikeReject("%s must be a boolean", name)
	}
}

// roguelikeIntField accepts only an unquoted JSON integer literal.
func roguelikeIntField(raw json.RawMessage, name string) (int, error) {
	n, err := operProgressInteger(raw)
	if err != nil {
		return 0, roguelikeReject("%s must be an integer", name)
	}
	return n, nil
}

// roguelikeStringField decodes one exact JSON string field. Empty,
// all-whitespace, NUL-containing, invalid UTF-8 and unpaired surrogate values
// are rejected; the decoded value is otherwise preserved verbatim.
func roguelikeStringField(raw json.RawMessage, name string, max int) (string, error) {
	s, err := operProgressDecodeString(raw)
	if err != nil {
		return "", roguelikeReject("%s must be a string", name)
	}
	if !validText(s, uint64(max), false) || strings.TrimSpace(s) == "" {
		return "", roguelikeReject("%s must be a nonempty string of at most %d bytes without NUL and not all whitespace", name, max)
	}
	return s, nil
}

// roguelikeEnumField validates a string field against a frozen value set.
func roguelikeEnumField(raw json.RawMessage, name string, allowed []string) (string, error) {
	s, err := roguelikeStringField(raw, name, MaxAdapterParamsBytes)
	if err != nil {
		return "", err
	}
	for _, id := range allowed {
		if s == id {
			return s, nil
		}
	}
	return "", roguelikeReject("%s must be one of %s", name, strings.Join(allowed, ", "))
}

// roguelikeOperListField validates a core_char_list-style array. Elements are
// objects with a required nonempty name and an optional boolean use_support;
// an empty array is valid and means no preferred operators.
func roguelikeOperListField(raw json.RawMessage, name string) error {
	items, err := operProgressJSONArray(raw)
	if err != nil {
		return roguelikeReject("%s must be an array", name)
	}
	for i, item := range items {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return roguelikeReject("%s entry %d must be an object", name, i)
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &entry); err != nil {
			return roguelikeReject("%s entry %d must be an object", name, i)
		}
		for key := range entry {
			if key != "name" && key != "use_support" {
				return roguelikeReject("%s entry %d has unknown field %q", name, i, key)
			}
		}
		nameRaw, ok := entry["name"]
		if !ok {
			return roguelikeReject("%s entry %d requires name", name, i)
		}
		operName, err := operProgressDecodeString(nameRaw)
		if err != nil || !validText(operName, operProgressMaxNameBytes, false) || strings.TrimSpace(operName) == "" {
			return roguelikeReject("%s entry %d name must be a nonempty string of at most %d bytes without NUL", name, i, operProgressMaxNameBytes)
		}
		if raw, ok := entry["use_support"]; ok {
			if _, err := roguelikeBoolField(raw, "use_support"); err != nil {
				return roguelikeReject("%s entry %d use_support must be a boolean", name, i)
			}
		}
	}
	return nil
}

// roguelikeFoldartalListField requires a nonempty array of nonempty foldartal
// names: Core skips empty entries and hard-rejects an all-empty list, so the
// adapter rejects both shapes up front.
func roguelikeFoldartalListField(raw json.RawMessage) error {
	items, err := operProgressJSONArray(raw)
	if err != nil {
		return roguelikeReject("start_foldartal_list must be an array")
	}
	if len(items) == 0 {
		return roguelikeReject("start_foldartal_list must not be empty")
	}
	for i, item := range items {
		name, err := operProgressDecodeString(item)
		if err != nil || !validText(name, MaxAdapterParamsBytes, false) || strings.TrimSpace(name) == "" {
			return roguelikeReject("start_foldartal_list entry %d must be a nonempty string without NUL", i)
		}
	}
	return nil
}

// roguelikeParadigmListField accepts an array of nonempty collapsal paradigm
// names; an empty array is valid and selects no paradigms.
func roguelikeParadigmListField(raw json.RawMessage) error {
	items, err := operProgressJSONArray(raw)
	if err != nil {
		return roguelikeReject("expected_collapsal_paradigms must be an array")
	}
	for i, item := range items {
		name, err := operProgressDecodeString(item)
		if err != nil || !validText(name, MaxAdapterParamsBytes, false) || strings.TrimSpace(name) == "" {
			return roguelikeReject("expected_collapsal_paradigms entry %d must be a nonempty string without NUL", i)
		}
	}
	return nil
}

// roguelikeBlackFlowAnimalsField validates the start-run animal list and
// returns the entry count (0-2) plus whether the feathered serpent is present.
func roguelikeBlackFlowAnimalsField(raw json.RawMessage) (int, bool, error) {
	items, err := operProgressJSONArray(raw)
	if err != nil {
		return 0, false, roguelikeReject("blackflow_start_animals must be an array")
	}
	if len(items) > 2 {
		return 0, false, roguelikeReject("blackflow_start_animals must contain 0-2 entries")
	}
	serpents := 0
	for i, item := range items {
		animal, err := operProgressDecodeString(item)
		if err != nil {
			return 0, false, roguelikeReject("blackflow_start_animals entry %d must be a string", i)
		}
		known := false
		for _, id := range roguelikeBlackFlowAnimals {
			if animal == id {
				known = true
				break
			}
		}
		if !known {
			return 0, false, roguelikeReject("blackflow_start_animals entry %d is not a known animal id", i)
		}
		if animal == "swaddled_feathered_serpent" {
			serpents++
		}
	}
	if serpents > 1 {
		return 0, false, roguelikeReject("blackflow_start_animals allows at most one swaddled_feathered_serpent")
	}
	return len(items), serpents > 0, nil
}

// roguelikeBlackFlowRewardPriorityField requires a nonempty array of distinct
// known reward ids.
func roguelikeBlackFlowRewardPriorityField(raw json.RawMessage) error {
	items, err := operProgressJSONArray(raw)
	if err != nil {
		return roguelikeReject("blackflow_start_reward_priority must be an array")
	}
	if len(items) == 0 {
		return roguelikeReject("blackflow_start_reward_priority must not be empty")
	}
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		id, err := operProgressDecodeString(item)
		if err != nil {
			return roguelikeReject("blackflow_start_reward_priority entry %d must be a string", i)
		}
		known := false
		for _, reward := range roguelikeBlackFlowRewardIDs {
			if id == reward {
				known = true
				break
			}
		}
		if !known {
			return roguelikeReject("blackflow_start_reward_priority entry %d is not a known reward id", i)
		}
		if seen[id] {
			return roguelikeReject("blackflow_start_reward_priority entry %d repeats reward id %q", i, id)
		}
		seen[id] = true
	}
	return nil
}

// roguelikeCollectibleStartListField validates the mode-4 start selection
// object. Sub-keys are booleans; key and dice belong to Mizuki, ideas to
// Sarkaz and ticket to JieGarden, and a misplaced sub-key is rejected instead
// of being silently ignored by Core.
func roguelikeCollectibleStartListField(raw json.RawMessage, theme string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return roguelikeReject("collectible_mode_start_list must be an object")
	}
	var list map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return roguelikeReject("collectible_mode_start_list must be an object")
	}
	for key, value := range list {
		allowed := key == "hot_water" || key == "shield" || key == "ingot" || key == "hope" || key == "random" ||
			(theme == roguelikeThemeMizuki && (key == "key" || key == "dice")) ||
			(theme == roguelikeThemeSarkaz && key == "ideas") ||
			(theme == roguelikeThemeJieGarden && key == "ticket")
		if !allowed {
			return roguelikeReject("collectible_mode_start_list has unknown field %q for theme %q", key, theme)
		}
		if _, err := roguelikeBoolField(value, key); err != nil {
			return roguelikeReject("collectible_mode_start_list field %s must be a boolean", key)
		}
	}
	return nil
}
