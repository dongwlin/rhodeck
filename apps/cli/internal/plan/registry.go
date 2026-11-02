// Package plan owns the plan-facing work of the CLI: reading one explicit
// plan, the local adapter registry, preparation, execution and installation
// diagnosis. It depends on the control package for the runtime and on the
// output package for the machine contract.
package plan

import (
	"fmt"
	"strings"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/copilot"
	"github.com/dongwlin/rhodeck/packages/control/adapters/operprogress"
	"github.com/dongwlin/rhodeck/packages/control/adapters/roguelike"
	"github.com/dongwlin/rhodeck/packages/control/adapters/switchtheme"
)

// Spec pairs one locally registered adapter with the Core type it accepts and
// the facts strict preparation cannot verify locally. Adapter identity in
// capability output comes from Descriptor, so name and version are declared
// once by the control package. One adapter's inputs can select several
// objective policies: ObjectivePolicies lists every policy its inputs may
// select in the fixed all, any, none order, and PolicySelection names the
// input that selects each one. ResultInterpretation names the highest
// capability those inputs reach; input-only branches never upgrade it.
type Spec struct {
	CoreType             string
	Input                string
	ObjectivePolicies    []control.ObjectivePolicy
	PolicySelection      string
	ResultInterpretation string
	// InputOnlyNote states which inputs select no policy and interpret no
	// results. It becomes part of the acceptance statement; adapters whose
	// every input selects a policy leave it empty.
	InputOnlyNote string
	Unverified    []string
	newAdapter    func() control.TaskAdapter
}

// Row builds the capability row of one spec. result_interpretation names the
// highest capability the adapter's inputs can reach; a purely input-only
// adapter reports none, and partially input-only adapters describe those
// branches as none in policy_selection, input and acceptance instead.
func (s Spec) Row() output.AdapterRow {
	ref := s.newAdapter().Descriptor()
	interpretation := s.ResultInterpretation
	acceptance := "local preparation and result interpretation only"
	switch {
	case len(s.ObjectivePolicies) == 1 && s.ObjectivePolicies[0] == control.ObjectiveNone:
		interpretation = "none"
		acceptance = "local preparation only; result interpretation: none"
	case s.InputOnlyNote != "":
		acceptance += "; " + s.InputOnlyNote
	}
	return output.AdapterRow{
		Name: ref.Name, Version: ref.Version, CoreType: s.CoreType, Input: s.Input,
		ObjectivePolicies: s.ObjectivePolicies, PolicySelection: s.PolicySelection,
		ResultInterpretation: interpretation, Acceptance: acceptance,
	}
}

// specs returns the locally registered adapters. The registry is the only
// place a task adapter enters the CLI.
func specs() []Spec {
	return []Spec{
		{
			CoreType:             "OperProgress",
			ObjectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll},
			PolicySelection:      "all inputs use policy all",
			ResultInterpretation: "target_results",
			Input:                "strict plans: 1-128 entries with role (Pioneer, Warrior, Tank, Sniper, Caster, Medic, Support, Special), name (valid UTF-8, 1-256 bytes, no NUL, not all whitespace) and exactly one of elite 1-2, skill_level 2-7 or skill_mastery (three 0-3 integers, exactly one nonzero slot); targets op-0001..op-0128",
			Unverified: []string{
				"oper_progress: operator identity, role agreement with installed battle data, skill existence, elite prerequisites, materials, inventory and training-room state",
				"oper_progress: a mastery Completed report means training at the target level started; it does not prove training finished or was independently confirmed",
			},
			newAdapter: operprogress.New,
		},
		{
			CoreType:             "SwitchTheme",
			ObjectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll},
			PolicySelection:      "all inputs use policy all",
			ResultInterpretation: "target_results",
			Input:                "strict themes: 1-32 distinct names (valid UTF-8, 1-256 bytes, no NUL, no leading or trailing whitespace); sole target theme-selection; Core selects one candidate at random once, not a retry or fallback sequence",
			Unverified: []string{
				"switch_theme: theme availability, unlock state and the final theme state",
				"switch_theme: multiple candidates are a single random selection, not a sequential retry, progressive-switch list or failure fallback",
			},
			newAdapter: switchtheme.New,
		},
		{
			CoreType:             "Copilot",
			ObjectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll, control.ObjectiveNone},
			PolicySelection:      "copilot_list inputs use policy all and interpret per-entry stage results, while single-file filename and loop_times inputs use policy none and interpret no results",
			ResultInterpretation: "stage_results",
			Input:                "strict params: exactly one of filename (nonempty path string) or copilot_list (1-128 objects, each a required nonempty filename plus an optional unique signed 32-bit id, optional nav_name_override and boolean is_raid); optional formation, formation_index 0-4, add_trust, ignore_requirements, use_sanity_potion, support_unit_usage 0-3 (usage 2 requires support_unit_name) and loop_times 1-2147483647 (copilot_list allows omission or 1); operbox_data_path and user_additional require formation=true; referenced files are never opened and single-file and loop inputs are input-only (none)",
			InputOnlyNote:        "single-file and loop inputs interpret no results (none); a completed single-file chain does not prove a clear or a safe device handoff",
			Unverified: []string{
				"copilot: file existence and contents, stage support, navigation and recognition against the installed resources, and formation or operbox state are never inspected during preparation",
				"copilot: load success, append acceptance and chain completion do not establish stage facts; a completed single-file or loop chain does not prove a clear or a safe device handoff",
			},
			newAdapter: copilot.New,
		},
		{
			CoreType:             "Roguelike",
			ObjectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll, control.ObjectiveAny, control.ObjectiveNone},
			PolicySelection:      "the theme and mode with starts_count and the rotation and early-stop switches select the policy: mode 0 uses all for one declared run and any for several runs (starts_count 1-128 with no early-stop switch and no reachable investment stop), mode 1 uses all only when an investment limit can be reached, mode 6 uses all only with monthly_squad_auto_iterate=true, mode 7 uses all only with deep_exploration_auto_iterate=true, modes 20001 and 30003 always use all, and every other configuration uses none",
			ResultInterpretation: "mode_objective",
			Input:                "strict params: an explicit theme (Phantom, Mizuki, Sami, Sarkaz, JieGarden or BlackFlow) and mode validated against the frozen per-theme matrix (0, 1, 4, 6 and 7 for every theme except BlackFlow; Sami adds 5, Sarkaz adds 10001, JieGarden adds 20001 and BlackFlow accepts only 0, 1, 30001 and 30003), common squad, investment and early-stop options and theme- or mode-scoped fields such as monthly_squad_auto_iterate, deep_exploration_auto_iterate, find_playTime_target and the blackflow_* set; unknown or misplaced fields and invalid combinations are rejected",
			InputOnlyNote:        "modes 6 and 7 with their rotation switch off, modes without a reliable terminal fact and the BlackFlow strategy profiles interpret no results (none)",
			Unverified: []string{
				"roguelike: theme availability, operator and squad names, seeds, foldartal and paradigm names and reward identifiers against the installed resources, and device readiness are never inspected during preparation",
				"roguelike: missing settlement evidence stays unobserved even after chain completion and is never inferred from finished tasks",
			},
			newAdapter: roguelike.New,
		},
	}
}

// Adapters returns the explicit, runtime-local adapter registry. The same
// slice feeds control.Config.Adapters and control.PreparePlan; a task without a
// matching Task.Adapter stays on the open raw path.
func Adapters() []control.TaskAdapter {
	specs := specs()
	adapters := make([]control.TaskAdapter, 0, len(specs))
	for _, spec := range specs {
		adapters = append(adapters, spec.newAdapter())
	}
	return adapters
}

// Rows derives the capability rows from the same descriptors the runner
// registers. Each row describes local preparation and result interpretation.
func Rows() []output.AdapterRow {
	specs := specs()
	out := make([]output.AdapterRow, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Row())
	}
	return out
}

// Summary lists the registered identities for help output. Names and versions
// come from Descriptor so help and capabilities cannot drift from the
// registry.
func Summary() string {
	parts := make([]string, 0, len(specs()))
	for _, spec := range specs() {
		ref := spec.newAdapter().Descriptor()
		parts = append(parts, fmt.Sprintf("%s@%d (%s)", ref.Name, ref.Version, spec.CoreType))
	}
	return strings.Join(parts, ", ")
}

// unverified returns the preview caveats for the adapters that one source plan
// actually selects. Raw plans add nothing because they exercise no
// adapter-specific validation.
func adapterUnverified(p control.Plan) []string {
	var out []string
	for _, spec := range specs() {
		ref := spec.newAdapter().Descriptor()
		for _, task := range p.Tasks {
			if task.Adapter != nil && *task.Adapter == ref {
				out = append(out, spec.Unverified...)
				break
			}
		}
	}
	return out
}
