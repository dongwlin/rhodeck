package cli

import (
	"fmt"
	"strings"

	"github.com/dongwlin/rhodeck/packages/control"
)

// adapterBasis records the implementation review behind the bundled adapters.
// It names the reviewed source revision and states that nothing was executed.
const adapterBasis = "MAA source 9b75f9f7b103079458414c6e20216c8c531d6987 static review; adapters were not executed"

// coreVersionPolicy describes installation selection without claiming that a
// source review verifies any installed library, resources or task capability.
const coreVersionPolicy = "rolling; use the latest caller-provided Core and resources; no fixed Core version or old-version compatibility guarantee; no automatic downloads or updates; only the actual combination verified by doctor/run/record is evidence"

// installationFacts states what capability discovery knows about an installed
// Core. Discovery takes no plan and never loads native code.
const installationFacts = "not inspected; capabilities performs no installation discovery or native loading; run doctor with an explicit plan to inspect an installation"

// deviceAcceptance is the current authorized acceptance state of the bundled
// adapters.
const deviceAcceptance = "none"

// cliAdapterSpec pairs one locally registered adapter with the Core type it
// accepts and the facts strict preparation cannot verify locally. Adapter
// identity in capability output comes from Descriptor, so name and version are
// declared once by the control package. One adapter's inputs can select
// several objective policies: objectivePolicies lists every policy its inputs
// may select in the fixed all, any, none order, and policySelection names the
// input that selects each one. resultInterpretation names the highest
// capability those inputs reach; input-only branches never upgrade it.
type cliAdapterSpec struct {
	coreType             string
	input                string
	objectivePolicies    []control.ObjectivePolicy
	policySelection      string
	resultInterpretation string
	// inputOnlyNote states which inputs select no policy and interpret no
	// results. It becomes part of the acceptance statement; adapters whose
	// every input selects a policy leave it empty.
	inputOnlyNote string
	unverified    []string
	newAdapter    func() control.TaskAdapter
}

// adapterCapability describes one registered adapter in machine output. The
// name and version are copied from Descriptor; the remaining fields document
// what the runner accepts, which objective policies its inputs can select and
// which evidence is still missing. There is deliberately no single static
// objective_policy field: dry-run reports the policy each prepared task
// actually selected.
type adapterCapability struct {
	Name                 string                    `json:"name"`
	Version              int                       `json:"version"`
	CoreType             string                    `json:"core_type"`
	Input                string                    `json:"input"`
	ObjectivePolicies    []control.ObjectivePolicy `json:"objective_policies"`
	PolicySelection      string                    `json:"policy_selection"`
	ResultInterpretation string                    `json:"result_interpretation"`
	Acceptance           string                    `json:"acceptance"`
}

func cliAdapterSpecs() []cliAdapterSpec {
	return []cliAdapterSpec{
		{
			coreType:             "OperProgress",
			objectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll},
			policySelection:      "all inputs use policy all",
			resultInterpretation: "target_results",
			input:                "strict plans: 1-128 entries with role (Pioneer, Warrior, Tank, Sniper, Caster, Medic, Support, Special), name (valid UTF-8, 1-256 bytes, no NUL, not all whitespace) and exactly one of elite 1-2, skill_level 2-7 or skill_mastery (three 0-3 integers, exactly one nonzero slot); targets op-0001..op-0128",
			unverified: []string{
				"oper_progress: operator identity, role agreement with installed battle data, skill existence, elite prerequisites, materials, inventory and training-room state",
				"oper_progress: target achievements and training-start reports have no authorized device acceptance",
				"oper_progress: a mastery Completed report means training at the target level started; it does not prove training finished or was independently confirmed",
			},
			newAdapter: control.NewOperProgressAdapter,
		},
		{
			coreType:             "SwitchTheme",
			objectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll},
			policySelection:      "all inputs use policy all",
			resultInterpretation: "target_results",
			input:                "strict themes: 1-32 distinct names (valid UTF-8, 1-256 bytes, no NUL, no leading or trailing whitespace); sole target theme-selection; Core selects one candidate at random once, not a retry or fallback sequence",
			unverified: []string{
				"switch_theme: theme availability, unlock state and the final theme state",
				"switch_theme: already-set, locked, not-found and confirmation observations have no authorized device acceptance",
				"switch_theme: multiple candidates are a single random selection, not a sequential retry, progressive-switch list or failure fallback; this is not device-verified",
			},
			newAdapter: control.NewSwitchThemeAdapter,
		},
		{
			coreType:             "Copilot",
			objectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll, control.ObjectiveNone},
			policySelection:      "copilot_list inputs use policy all and interpret per-entry stage results, while single-file filename and loop_times inputs use policy none and interpret no results",
			resultInterpretation: "stage_results",
			input:                "strict params: exactly one of filename (nonempty path string) or copilot_list (1-128 objects, each a required nonempty filename plus an optional unique signed 32-bit id, optional nav_name_override and boolean is_raid); optional formation, formation_index 0-4, add_trust, ignore_requirements, use_sanity_potion, support_unit_usage 0-3 (usage 2 requires support_unit_name) and loop_times 1-2147483647 (copilot_list allows omission or 1); operbox_data_path and user_additional require formation=true; referenced files are never opened and single-file and loop inputs are input-only (none)",
			inputOnlyNote:        "single-file and loop inputs interpret no results (none); a completed single-file chain does not prove a clear or a safe device handoff",
			unverified: []string{
				"copilot: file existence and contents, stage support, navigation and recognition against the installed resources, and formation or operbox state are never inspected during preparation",
				"copilot: per-entry clear and failure facts have no authorized device acceptance",
				"copilot: load success, append acceptance and chain completion do not establish stage facts; a completed single-file or loop chain does not prove a clear or a safe device handoff",
			},
			newAdapter: control.NewCopilotAdapter,
		},
		{
			coreType:             "Roguelike",
			objectivePolicies:    []control.ObjectivePolicy{control.ObjectiveAll, control.ObjectiveAny, control.ObjectiveNone},
			policySelection:      "the theme and mode with starts_count and the rotation and early-stop switches select the policy: mode 0 uses all for one declared run and any for several runs (starts_count 1-128 with no early-stop switch and no reachable investment stop), mode 1 uses all only when an investment limit can be reached, mode 6 uses all only with monthly_squad_auto_iterate=true, mode 7 uses all only with deep_exploration_auto_iterate=true, modes 20001 and 30003 always use all, and every other configuration uses none",
			resultInterpretation: "mode_objective",
			input:                "strict params: an explicit theme (Phantom, Mizuki, Sami, Sarkaz, JieGarden or BlackFlow) and mode validated against the frozen per-theme matrix (0, 1, 4, 6 and 7 for every theme except BlackFlow; Sami adds 5, Sarkaz adds 10001, JieGarden adds 20001 and BlackFlow accepts only 0, 1, 30001 and 30003), common squad, investment and early-stop options and theme- or mode-scoped fields such as monthly_squad_auto_iterate, deep_exploration_auto_iterate, find_playTime_target and the blackflow_* set; unknown or misplaced fields and invalid combinations are rejected",
			inputOnlyNote:        "modes 6 and 7 with their rotation switch off, modes without a reliable terminal fact and the BlackFlow strategy profiles interpret no results (none)",
			unverified: []string{
				"roguelike: theme availability, operator and squad names, seeds, foldartal and paradigm names and reward identifiers against the installed resources, and device readiness are never inspected during preparation",
				"roguelike: run settlement, investment, rotation, target-found and start-run facts have no authorized device acceptance",
				"roguelike: missing settlement evidence stays unobserved even after chain completion and is never inferred from finished tasks",
			},
			newAdapter: control.NewRoguelikeAdapter,
		},
	}
}

// cliAdapters returns the explicit, runtime-local adapter registry. The same
// slice feeds control.Config.Adapters and control.PreparePlan; a task without a
// matching Task.Adapter stays on the open raw path.
func cliAdapters() []control.TaskAdapter {
	specs := cliAdapterSpecs()
	adapters := make([]control.TaskAdapter, 0, len(specs))
	for _, spec := range specs {
		adapters = append(adapters, spec.newAdapter())
	}
	return adapters
}

// cliAdapterCapabilities derives capability rows from the same descriptors the
// runner registers. Acceptance text never claims device or real-Core proof.
func cliAdapterCapabilities() []adapterCapability {
	specs := cliAdapterSpecs()
	out := make([]adapterCapability, 0, len(specs))
	for _, spec := range specs {
		out = append(out, cliAdapterCapability(spec))
	}
	return out
}

// cliAdapterCapability builds one capability row. result_interpretation names
// the highest capability the adapter's inputs can reach; a purely input-only
// adapter reports none, and partially input-only adapters describe those
// branches as none in policy_selection, input and acceptance instead.
func cliAdapterCapability(spec cliAdapterSpec) adapterCapability {
	ref := spec.newAdapter().Descriptor()
	interpretation := spec.resultInterpretation
	acceptance := "local preparation and result interpretation only"
	switch {
	case len(spec.objectivePolicies) == 1 && spec.objectivePolicies[0] == control.ObjectiveNone:
		interpretation = "none"
		acceptance = "local preparation only; result interpretation: none"
	case spec.inputOnlyNote != "":
		acceptance += "; " + spec.inputOnlyNote
	}
	return adapterCapability{
		Name: ref.Name, Version: ref.Version, CoreType: spec.coreType, Input: spec.input,
		ObjectivePolicies: spec.objectivePolicies, PolicySelection: spec.policySelection,
		ResultInterpretation: interpretation, Acceptance: acceptance + "; authorized device acceptance: " + deviceAcceptance,
	}
}

// cliAdapterSummary lists the registered identities for help output. Names and
// versions come from Descriptor so help and capabilities cannot drift from the
// registry.
func cliAdapterSummary() string {
	specs := cliAdapterSpecs()
	parts := make([]string, 0, len(specs))
	for _, spec := range specs {
		ref := spec.newAdapter().Descriptor()
		parts = append(parts, fmt.Sprintf("%s@%d (%s)", ref.Name, ref.Version, spec.coreType))
	}
	return strings.Join(parts, ", ")
}

// adapterUnverified returns the preview caveats for the adapters that this
// source plan actually selects. Raw plans add nothing because they exercise no
// adapter-specific validation.
func adapterUnverified(p control.Plan) []string {
	specs := cliAdapterSpecs()
	var out []string
	for _, spec := range specs {
		ref := spec.newAdapter().Descriptor()
		selected := false
		for _, task := range p.Tasks {
			if task.Adapter != nil && *task.Adapter == ref {
				selected = true
				break
			}
		}
		if selected {
			out = append(out, spec.unverified...)
		}
	}
	return out
}
