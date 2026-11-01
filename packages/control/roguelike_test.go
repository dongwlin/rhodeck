package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func roguelikeTask(params string) Task {
	return Task{ID: "roguelike-task", Type: roguelikeTaskType, Params: json.RawMessage(params)}
}

func TestRoguelikePrepareAcceptsStrictInputs(t *testing.T) {
	for _, params := range []string{
		`{"theme":"Phantom","mode":0}`,
		`{"theme":"Phantom","mode":1,"investment_with_more_score":true,"stop_when_investment_full":true,"starts_count":0,"investments_count":0}`,
		`{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":true,"monthly_squad_check_comms":false}`,
		`{"theme":"Phantom","mode":7,"deep_exploration_auto_iterate":true}`,
		`{"theme":"Phantom","mode":0,"difficulty":2147483647,"starts_count":9999999999}`,
		`{"theme":"Phantom","mode":4,"collectible_mode_start_list":{}}`,
		`{"theme":"Phantom","mode":0,"core_char_list":[]}`,
		`{"theme":"Phantom","mode":0,"core_char_list":[{"name":" Terra"},{"name":"Exusiai","use_support":true}]}`,
		`{"theme":"Phantom","mode":0,"squad":" padded "}`,
		`{"theme":"Mizuki","mode":0,"refresh_trader_with_dice":true}`,
		`{"theme":"Mizuki","mode":4,"collectible_mode_start_list":{"hot_water":true,"key":true,"dice":false}}`,
		`{"theme":"Sami","mode":5,"check_collapsal_paradigms":true,"double_check_collapsal_paradigms":true,"expected_collapsal_paradigms":["目空一些"],"use_foldartal":false}`,
		`{"theme":"Sami","mode":4,"start_foldartal_list":["远见"],"first_floor_foldartal":"远见"}`,
		`{"theme":"Sami","mode":5,"expected_collapsal_paradigms":[]}`,
		`{"theme":"Sami","mode":0,"difficulty":-1}`,
		`{"theme":"Sarkaz","mode":10001,"start_with_seed":"PRTXS-SEED"}`,
		`{"theme":"Sarkaz","mode":4,"start_with_elite_two":true,"only_start_with_elite_two":true,"collectible_mode_start_list":{"ideas":true,"shield":false}}`,
		`{"theme":"JieGarden","mode":20001,"find_playTime_target":3}`,
		`{"theme":"JieGarden","mode":1,"squad":"指挥分队","difficulty":3}`,
		`{"theme":"JieGarden","mode":4,"collectible_mode_start_list":{"ticket":true}}`,
		`{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn","blackflow_diagnostics":"full","blackflow_diagnostic_image_limit":100}`,
		`{"theme":"BlackFlow","mode":1,"blackflow_strategy":"investment","blackflow_cultivation_target":"swaddled_cerberus"}`,
		`{"theme":"BlackFlow","mode":30001,"blackflow_strategy":"baby_animal_floor3"}`,
		`{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":0}`,
		`{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_feathered_serpent"],"blackflow_start_rewards":4095}`,
		`{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat","swaddled_dog"],"blackflow_start_rewards":63,"blackflow_start_utopia":true,"blackflow_start_reward_priority":["unnumbered_item","pegasus","golden_crow"],"collectible_mode_squad":"指挥分队","collectible_mode_roles":"Sniper","collectible_mode_core_char_list":[{"name":"Exusiai","use_support":true}]}`,
		`{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1,"blackflow_start_reward_priority":["pegasus"]}`,
	} {
		t.Run(params, func(t *testing.T) {
			prepared, err := NewRoguelikeAdapter().Prepare(roguelikeTask(params))
			if err != nil {
				t.Fatal(err)
			}
			switch prepared.Policy {
			case ObjectiveNone:
				if len(prepared.Targets) != 0 || prepared.Reducer != nil {
					t.Fatalf("input-only preparation = %+v", prepared)
				}
			case ObjectiveAll, ObjectiveAny:
				if len(prepared.Targets) == 0 || prepared.Reducer == nil {
					t.Fatalf("interpreted preparation = %+v", prepared)
				}
			default:
				t.Fatalf("unexpected objective policy %q", prepared.Policy)
			}
			if !validObject(prepared.Params, MaxAdapterParamsBytes) {
				t.Fatalf("compiled params are invalid: %s", prepared.Params)
			}
		})
	}
}

func TestRoguelikePrepareRejectsInvalidInputs(t *testing.T) {
	for name, params := range map[string]string{
		"not object":                `[]`,
		"null root":                 `null`,
		"string root":               `"x"`,
		"trailing input":            `{"theme":"Phantom","mode":0}}`,
		"empty params":              `{}`,
		"missing theme":             `{"mode":0}`,
		"missing mode":              `{"theme":"Phantom"}`,
		"unknown theme":             `{"theme":"Liang","mode":0}`,
		"theme case":                `{"theme":"phantom","mode":0}`,
		"theme number":              `{"theme":1,"mode":0}`,
		"theme bool":                `{"theme":true,"mode":0}`,
		"mode string":               `{"theme":"Phantom","mode":"0"}`,
		"mode fraction":             `{"theme":"Phantom","mode":1.0}`,
		"mode exponent":             `{"theme":"Phantom","mode":1e0}`,
		"mode bool":                 `{"theme":"Phantom","mode":true}`,
		"mode 2":                    `{"theme":"Phantom","mode":2}`,
		"mode 3":                    `{"theme":"Phantom","mode":3}`,
		"mode 5 phantom":            `{"theme":"Phantom","mode":5}`,
		"mode 5 blackflow":          `{"theme":"BlackFlow","mode":5}`,
		"mode 4 blackflow":          `{"theme":"BlackFlow","mode":4}`,
		"mode 6 blackflow":          `{"theme":"BlackFlow","mode":6}`,
		"mode 7 blackflow":          `{"theme":"BlackFlow","mode":7}`,
		"mode 10001 phantom":        `{"theme":"Phantom","mode":10001}`,
		"mode 10001 jiegarden":      `{"theme":"JieGarden","mode":10001}`,
		"mode 20001 sarkaz":         `{"theme":"Sarkaz","mode":20001}`,
		"mode 30001 phantom":        `{"theme":"Phantom","mode":30001}`,
		"mode 30003 sami":           `{"theme":"Sami","mode":30003}`,
		"unknown top":               `{"theme":"Phantom","mode":0,"future":true}`,
		"case variant":              `{"theme":"Phantom","mode":0,"Mode":1}`,
		"enable rejected":           `{"theme":"Phantom","mode":0,"enable":true}`,
		"duplicate theme":           `{"theme":"Phantom","theme":"Mizuki","mode":0}`,
		"duplicate mode":            `{"theme":"Phantom","mode":0,"mode":1}`,
		"duplicate entry key":       `{"theme":"Phantom","mode":0,"core_char_list":[{"name":"a","name":"b"}]}`,
		"duplicate deep key":        `{"theme":"Sarkaz","mode":4,"collectible_mode_start_list":{"ideas":true,"ideas":false}}`,
		"elite two mode 1":          `{"theme":"Phantom","mode":1,"start_with_elite_two":true}`,
		"only mode 0":               `{"theme":"Sami","mode":0,"only_start_with_elite_two":true}`,
		"only without start":        `{"theme":"Phantom","mode":4,"only_start_with_elite_two":true}`,
		"only start false":          `{"theme":"Phantom","mode":4,"start_with_elite_two":false,"only_start_with_elite_two":true}`,
		"difficulty low":            `{"theme":"Phantom","mode":0,"difficulty":-2}`,
		"difficulty high":           `{"theme":"Phantom","mode":0,"difficulty":21}`,
		"difficulty near max":       `{"theme":"Phantom","mode":0,"difficulty":2147483646}`,
		"difficulty fraction":       `{"theme":"Phantom","mode":0,"difficulty":1.5}`,
		"difficulty string":         `{"theme":"Phantom","mode":0,"difficulty":"1"}`,
		"starts negative":           `{"theme":"Phantom","mode":0,"starts_count":-1}`,
		"investments negative":      `{"theme":"Phantom","mode":0,"investments_count":-1}`,
		"starts fraction":           `{"theme":"Phantom","mode":0,"starts_count":1.0}`,
		"squad empty":               `{"theme":"Phantom","mode":0,"squad":""}`,
		"squad whitespace":          `{"theme":"Phantom","mode":0,"squad":"  "}`,
		"squad number":              `{"theme":"Phantom","mode":0,"squad":1}`,
		"core_char empty":           `{"theme":"Phantom","mode":0,"core_char":""}`,
		"core_char_list object":     `{"theme":"Phantom","mode":0,"core_char_list":{}}`,
		"core_char_list string":     `{"theme":"Phantom","mode":0,"core_char_list":["Exusiai"]}`,
		"core_char_list null entry": `{"theme":"Phantom","mode":0,"core_char_list":[null]}`,
		"entry missing name":        `{"theme":"Phantom","mode":0,"core_char_list":[{"use_support":true}]}`,
		"entry empty name":          `{"theme":"Phantom","mode":0,"core_char_list":[{"name":""}]}`,
		"entry support number":      `{"theme":"Phantom","mode":0,"core_char_list":[{"name":"Exusiai","use_support":1}]}`,
		"entry unknown field":       `{"theme":"Phantom","mode":0,"core_char_list":[{"name":"Exusiai","future":1}]}`,
		"refresh phantom":           `{"theme":"Phantom","mode":0,"refresh_trader_with_dice":true}`,
		"refresh number":            `{"theme":"Mizuki","mode":0,"refresh_trader_with_dice":1}`,
		"sami field phantom":        `{"theme":"Phantom","mode":0,"use_foldartal":true}`,
		"sami field blackflow":      `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn","use_foldartal":true}`,
		"foldartal string":          `{"theme":"Sami","mode":0,"start_foldartal_list":"远见"}`,
		"foldartal empty":           `{"theme":"Sami","mode":0,"start_foldartal_list":[]}`,
		"foldartal empty entry":     `{"theme":"Sami","mode":0,"start_foldartal_list":[""]}`,
		"foldartal blank entry":     `{"theme":"Sami","mode":0,"start_foldartal_list":[" "]}`,
		"foldartal number entry":    `{"theme":"Sami","mode":0,"start_foldartal_list":[1]}`,
		"foldartal null entry":      `{"theme":"Sami","mode":0,"start_foldartal_list":[null]}`,
		"paradigm number entry":     `{"theme":"Sami","mode":0,"expected_collapsal_paradigms":[1]}`,
		"paradigm empty entry":      `{"theme":"Sami","mode":0,"expected_collapsal_paradigms":[""]}`,
		"paradigm object":           `{"theme":"Sami","mode":0,"expected_collapsal_paradigms":{}}`,
		"first_floor bool":          `{"theme":"Sami","mode":4,"first_floor_foldartal":true}`,
		"seed mizuki":               `{"theme":"Mizuki","mode":0,"start_with_seed":"x"}`,
		"seed bool":                 `{"theme":"Sarkaz","mode":0,"start_with_seed":true}`,
		"seed empty":                `{"theme":"Sarkaz","mode":0,"start_with_seed":""}`,
		"start list mode 1":         `{"theme":"Phantom","mode":1,"collectible_mode_shopping":true}`,
		"collectible squad mode 0":  `{"theme":"Phantom","mode":0,"collectible_mode_squad":"指挥分队"}`,
		"start list mode 6":         `{"theme":"Phantom","mode":6,"collectible_mode_start_list":{}}`,
		"start list array":          `{"theme":"Phantom","mode":4,"collectible_mode_start_list":[]}`,
		"start list unknown key":    `{"theme":"Phantom","mode":4,"collectible_mode_start_list":{"future":true}}`,
		"start list key phantom":    `{"theme":"Phantom","mode":4,"collectible_mode_start_list":{"key":true}}`,
		"start list ideas mizuki":   `{"theme":"Mizuki","mode":4,"collectible_mode_start_list":{"ideas":true}}`,
		"start list ticket sami":    `{"theme":"Sami","mode":4,"collectible_mode_start_list":{"ticket":true}}`,
		"start list number value":   `{"theme":"Phantom","mode":4,"collectible_mode_start_list":{"hot_water":1}}`,
		"start list null value":     `{"theme":"Phantom","mode":4,"collectible_mode_start_list":{"hope":null}}`,
		"monthly mode 0":            `{"theme":"Phantom","mode":0,"monthly_squad_auto_iterate":true}`,
		"monthly mode 7":            `{"theme":"Sami","mode":7,"monthly_squad_check_comms":true}`,
		"monthly number":            `{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":1}`,
		"deep mode 6":               `{"theme":"Phantom","mode":6,"deep_exploration_auto_iterate":true}`,
		"playtime mode 0":           `{"theme":"JieGarden","mode":0,"find_playTime_target":2}`,
		"playtime missing":          `{"theme":"JieGarden","mode":20001}`,
		"playtime zero":             `{"theme":"JieGarden","mode":20001,"find_playTime_target":0}`,
		"playtime high":             `{"theme":"JieGarden","mode":20001,"find_playTime_target":4}`,
		"playtime fraction":         `{"theme":"JieGarden","mode":20001,"find_playTime_target":2.5}`,
		"playtime string":           `{"theme":"JieGarden","mode":20001,"find_playTime_target":"2"}`,
		"blackflow field phantom":   `{"theme":"Phantom","mode":0,"blackflow_diagnostics":"full"}`,
		"collectible roles jie":     `{"theme":"JieGarden","mode":1,"collectible_mode_roles":"Sniper"}`,
		"collectible chars phantom": `{"theme":"Phantom","mode":0,"collectible_mode_core_char_list":[]}`,
		"strategy unknown":          `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn_x"}`,
		"strategy empty":            `{"theme":"BlackFlow","mode":0,"blackflow_strategy":""}`,
		"strategy number":           `{"theme":"BlackFlow","mode":0,"blackflow_strategy":1}`,
		"strategy 30003":            `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1,"blackflow_strategy":"burn"}`,
		"bare 30001":                `{"theme":"BlackFlow","mode":30001}`,
		"cultivation unknown":       `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"baby_animal","blackflow_cultivation_target":"swaddled_bird"}`,
		"diagnostics unknown":       `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn","blackflow_diagnostics":"verbose"}`,
		"image limit low":           `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn","blackflow_diagnostic_image_limit":-1}`,
		"image limit high":          `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn","blackflow_diagnostic_image_limit":101}`,
		"image limit fraction":      `{"theme":"BlackFlow","mode":0,"blackflow_strategy":"burn","blackflow_diagnostic_image_limit":1.5}`,
		"animals object":            `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":{},"blackflow_start_rewards":0}`,
		"animals three":             `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat","swaddled_dog","swaddled_cat"],"blackflow_start_rewards":0}`,
		"animals unknown":           `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_bird"],"blackflow_start_rewards":0}`,
		"animals number entry":      `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[1],"blackflow_start_rewards":0}`,
		"animals two serpents":      `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_feathered_serpent","swaddled_feathered_serpent"],"blackflow_start_rewards":0}`,
		"30003 missing animals":     `{"theme":"BlackFlow","mode":30003,"blackflow_start_rewards":0}`,
		"30003 missing rewards":     `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"]}`,
		"rewards low":               `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":-1}`,
		"rewards high":              `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":4096}`,
		"rewards fraction":          `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":1.0}`,
		"rewards string":            `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":"0"}`,
		"rewards high no serpent":   `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":4095}`,
		"rewards bit6 no serpent":   `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_dog"],"blackflow_start_rewards":64}`,
		"rewards high empty list":   `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":4095}`,
		"utopia without priority":   `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1,"blackflow_start_utopia":true}`,
		"priority empty":            `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":0,"blackflow_start_reward_priority":[]}`,
		"priority duplicate":        `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":0,"blackflow_start_reward_priority":["pegasus","pegasus"]}`,
		"priority unknown":          `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":0,"blackflow_start_reward_priority":["martian"]}`,
		"priority number entry":     `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":0,"blackflow_start_reward_priority":[1]}`,
		"priority object":           `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":[],"blackflow_start_rewards":0,"blackflow_start_reward_priority":{}}`,
		"second floor mode 1":       `{"theme":"Phantom","mode":1,"investment_enter_second_floor":true}`,
		"second floor mode 0":       `{"theme":"Phantom","mode":0,"investment_enter_second_floor":false}`,
		"nul squad":                 `{"theme":"Phantom","mode":0,"squad":"a\u0000b"}`,
		"unpaired high surrogate":   `{"theme":"Phantom","mode":0,"squad":"\ud800"}`,
		"unpaired low surrogate":    `{"theme":"Phantom","mode":0,"roles":"\udc00"}`,
		"invalid utf8":              "{\"theme\":\"Phantom\",\"mode\":0,\"squad\":\"\xff\"}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRoguelikeAdapter().Prepare(roguelikeTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
	for _, field := range []string{"theme", "mode", "squad", "core_char_list", "start_with_elite_two", "blackflow_strategy", "blackflow_start_animals", "blackflow_start_rewards"} {
		t.Run("null "+field, func(t *testing.T) {
			var params string
			switch field {
			case "theme", "mode":
				params = `{"` + field + `":null}`
			case "start_with_elite_two":
				params = `{"theme":"Phantom","mode":4,"start_with_elite_two":null}`
			case "blackflow_strategy":
				params = `{"theme":"BlackFlow","mode":0,"blackflow_strategy":null}`
			case "blackflow_start_animals", "blackflow_start_rewards":
				params = `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":0,"` + field + `":null}`
			default:
				params = `{"theme":"Phantom","mode":0,"` + field + `":null}`
			}
			if _, err := NewRoguelikeAdapter().Prepare(roguelikeTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("null input accepted: %v", err)
			}
		})
	}
}

func TestRoguelikeModeMatrix(t *testing.T) {
	requires := map[int]string{
		20001: `,"find_playTime_target":2`,
		30001: `,"blackflow_strategy":"burn"`,
		30003: `,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1`,
	}
	valid := map[string]map[int]bool{
		"Phantom":   {0: true, 1: true, 4: true, 6: true, 7: true},
		"Mizuki":    {0: true, 1: true, 4: true, 6: true, 7: true},
		"Sami":      {0: true, 1: true, 4: true, 5: true, 6: true, 7: true},
		"Sarkaz":    {0: true, 1: true, 4: true, 6: true, 7: true, 10001: true},
		"JieGarden": {0: true, 1: true, 4: true, 6: true, 7: true, 20001: true},
		"BlackFlow": {0: true, 1: true, 30001: true, 30003: true},
	}
	probes := []int{0, 1, 2, 3, 4, 5, 6, 7, 9, -1, 10001, 20001, 30001, 30003}
	adapter := NewRoguelikeAdapter()
	for _, theme := range roguelikeThemes {
		for _, mode := range probes {
			params := fmt.Sprintf(`{"theme":%q,"mode":%d%s}`, theme, mode, requires[mode])
			_, err := adapter.Prepare(roguelikeTask(params))
			if valid[theme][mode] {
				if err != nil {
					t.Fatalf("theme %s mode %d rejected: %v", theme, mode, err)
				}
				continue
			}
			if !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("theme %s mode %d accepted: %v", theme, mode, err)
			}
		}
	}
}

func TestRoguelikeCompilePreservesProvidedFields(t *testing.T) {
	const input = `{"theme":"Sarkaz","mode":4,"difficulty":20,"squad":"蓝图测绘分队","roles":"Caster","core_char":"Exusiai","core_char_list":[{"name":"Exusiai","use_support":true}],"use_support":false,"use_nonfriend_support":true,"starts_count":3,"stop_at_final_boss":true,"stop_at_max_level":false,"investment_enabled":false,"investments_count":9007199254740993,"investment_with_more_score":true,"stop_when_investment_full":true,"start_with_elite_two":true,"only_start_with_elite_two":true,"collectible_mode_shopping":true,"collectible_mode_squad":"指挥分队","collectible_mode_start_list":{"ideas":true}}`
	params, cfg, err := compileRoguelike(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(params, []byte("9007199254740993")) {
		t.Fatalf("integer literal did not survive compilation: %s", params)
	}
	var source, compiled map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &source); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(params, &compiled); err != nil {
		t.Fatal(err)
	}
	if len(compiled) != len(source) {
		t.Fatalf("compiled params injected or dropped fields: %s", params)
	}
	for field, want := range source {
		if !bytes.Equal(compiled[field], want) {
			t.Fatalf("field %s changed: %s, want %s", field, compiled[field], want)
		}
	}
	if cfg.Theme != "Sarkaz" || cfg.Mode != 4 {
		t.Fatalf("compiled config lost theme or mode: %+v", cfg)
	}
	for range 5 {
		again, againCfg, err := compileRoguelike(json.RawMessage(input))
		if err != nil || !bytes.Equal(again, params) || againCfg != cfg {
			t.Fatalf("compilation was not deterministic: %s, %+v, %v", again, againCfg, err)
		}
	}
}

func TestRoguelikeCompileCapturesObjectiveSwitches(t *testing.T) {
	_, cfg, err := compileRoguelike(json.RawMessage(`{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MonthlySquadAutoIterate || cfg.DeepExplorationAutoIterate || cfg.FindPlayTimeTarget != 0 {
		t.Fatalf("mode 6 switches = %+v", cfg)
	}
	_, cfg, err = compileRoguelike(json.RawMessage(`{"theme":"JieGarden","mode":20001,"find_playTime_target":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FindPlayTimeTarget != 2 || cfg.MonthlySquadAutoIterate || cfg.DeepExplorationAutoIterate {
		t.Fatalf("mode 20001 switches = %+v", cfg)
	}
	_, cfg, err = compileRoguelike(json.RawMessage(`{"theme":"Phantom","mode":7,"deep_exploration_auto_iterate":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DeepExplorationAutoIterate {
		t.Fatalf("mode 7 switches = %+v", cfg)
	}
	// Omitted switches keep the effective Core defaults for later
	// interpretation, while the compiled params stay free of injected values.
	params, cfg, err := compileRoguelike(json.RawMessage(`{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InvestmentEnabled || cfg.BlackFlowStartAnimals != 1 || cfg.BlackFlowUtopia || cfg.BlackFlowStartSerpent {
		t.Fatalf("start-run switches = %+v", cfg)
	}
	if bytes.Contains(params, []byte("investment_enabled")) {
		t.Fatalf("compiled params injected a default: %s", params)
	}
	_, cfg, err = compileRoguelike(json.RawMessage(`{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_feathered_serpent"],"blackflow_start_rewards":4095}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BlackFlowStartSerpent {
		t.Fatalf("start-run serpent switch = %+v", cfg)
	}
}

func TestRoguelikePrepareBudgets(t *testing.T) {
	for name, params := range map[string]string{
		"input bytes":    `{"theme":"Phantom","mode":0,"squad":"` + strings.Repeat("x", MaxAdapterParamsBytes) + `"}`,
		"compiled bytes": `{"theme":"Phantom","mode":0,"squad":"` + strings.Repeat("<", MaxAdapterParamsBytes/6) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRoguelikeAdapter().Prepare(roguelikeTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("over-budget input accepted: %v", err)
			}
		})
	}
}

func TestRoguelikePrepareIsDetachedAndConcurrent(t *testing.T) {
	const input = `{"theme":"Sami","mode":4,"start_foldartal_list":["远见"]}`
	adapter := NewRoguelikeAdapter()
	if got := adapter.Descriptor(); got != (AdapterRef{Name: "roguelike", Version: 1}) {
		t.Fatalf("descriptor = %+v", got)
	}
	task := roguelikeTask(input)
	first, err := adapter.Prepare(task)
	if err != nil {
		t.Fatal(err)
	}
	task.Params[2] = 'X'
	if bytes.Contains(first.Params, []byte("X")) {
		t.Fatal("compiled params alias input")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prepared, err := adapter.Prepare(roguelikeTask(input))
			if err != nil || !bytes.Equal(prepared.Params, first.Params) {
				t.Errorf("concurrent preparation changed output: %s, %v", prepared.Params, err)
			}
		}()
	}
	wg.Wait()
	if _, err := adapter.Prepare(Task{Type: "RoguelikeSpoils", Params: json.RawMessage(input)}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("wrong Core task type accepted: %v", err)
	}
}

func TestRoguelikeRawPlanRemainsOpen(t *testing.T) {
	plan := validTestPlan(t)
	ref := NewRoguelikeAdapter().Descriptor()
	const raw = `{"theme":"Phantom","mode":9,"future":{"n":9007199254740993}}`
	plan.Tasks = []Task{
		{ID: "raw", Type: "Roguelike", Params: json.RawMessage(raw)},
		{ID: "strict", Type: "Roguelike", Adapter: &ref, Params: json.RawMessage(`{"theme":"Phantom","mode":0}`)},
	}
	prepared, err := PreparePlan(plan, NewRoguelikeAdapter())
	if err != nil {
		t.Fatal(err)
	}
	if string(prepared.Plan.Tasks[0].Params) != raw || prepared.Tasks[0].Policy != "" || prepared.Tasks[0].Typed != nil {
		t.Fatalf("raw task changed: %+v", prepared.Tasks[0])
	}
	if prepared.Tasks[1].Policy != ObjectiveNone || prepared.Tasks[1].Typed != nil || len(prepared.reducers) != 0 {
		t.Fatalf("input-only preview = %+v", prepared.Tasks[1])
	}
}

func ExampleNewRoguelikeAdapter() {
	adapter := NewRoguelikeAdapter()
	prepared, err := adapter.Prepare(Task{ID: "burn", Type: "Roguelike", Params: json.RawMessage(`{"theme":"BlackFlow","mode":1,"blackflow_strategy":"investment"}`)})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(prepared.Params))
	fmt.Println(prepared.Policy)
	// Output:
	// {"blackflow_strategy":"investment","mode":1,"theme":"BlackFlow"}
	// none
}
