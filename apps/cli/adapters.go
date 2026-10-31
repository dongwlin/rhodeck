package main

import (
	"fmt"
	"strings"

	"github.com/dongwlin/rhodeck/packages/control"
)

// adapterBasis records the implementation review behind the bundled adapters.
// It names the reviewed source revision and states that nothing was executed.
const adapterBasis = "MAA source 9b75f9f7b103079458414c6e20216c8c531d6987 static review; adapters were not executed"

// staticSnapshotBasis separates the release candidate's library and resource
// snapshot from the reviewed source revision.
const staticSnapshotBasis = "MAA v6.20.1 release candidate is a separate library and resource snapshot; not executed"

// installationFacts states what capability discovery knows about an installed
// Core. Discovery takes no plan and never loads native code.
const installationFacts = "not inspected; capabilities performs no installation discovery or native loading; run doctor with an explicit plan to inspect an installation"

// deviceAcceptance is the current authorized acceptance state of both adapters.
const deviceAcceptance = "none"

// cliAdapterSpec pairs one locally registered adapter with the Core type it
// accepts and the facts strict preparation cannot verify locally. Adapter
// identity in capability output comes from Descriptor, so name and version are
// declared once by the control package.
type cliAdapterSpec struct {
	coreType   string
	input      string
	unverified []string
	newAdapter func() control.TaskAdapter
}

// adapterCapability describes one registered adapter in machine output. The
// name and version are copied from Descriptor; the remaining fields document
// what the runner accepts and which evidence is still missing.
type adapterCapability struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	CoreType   string `json:"core_type"`
	Input      string `json:"input"`
	Acceptance string `json:"acceptance"`
}

func cliAdapterSpecs() []cliAdapterSpec {
	return []cliAdapterSpec{
		{
			coreType: "OperProgress",
			input:    "strict plans: 1-128 entries with role (Pioneer, Warrior, Tank, Sniper, Caster, Medic, Support, Special), name (valid UTF-8, 1-256 bytes, no NUL, not all whitespace) and exactly one of elite 1-2, skill_level 2-7 or skill_mastery (three 0-3 integers, exactly one nonzero slot); targets op-0001..op-0128",
			unverified: []string{
				"oper_progress: operator identity, role agreement with installed battle data, skill existence, elite prerequisites, materials, inventory and training-room state",
				"oper_progress: target achievements and training-start reports have no authorized device acceptance",
				"oper_progress: a mastery Completed report means training at the target level started; it does not prove training finished or was independently confirmed",
			},
			newAdapter: control.NewOperProgressAdapter,
		},
		{
			coreType: "SwitchTheme",
			input:    "strict themes: 1-32 distinct names (valid UTF-8, 1-256 bytes, no NUL, no leading or trailing whitespace); sole target theme-selection; Core selects one candidate at random once, not a retry or fallback sequence",
			unverified: []string{
				"switch_theme: theme availability, unlock state and the final theme state",
				"switch_theme: already-set, locked, not-found and confirmation observations have no authorized device acceptance",
				"switch_theme: multiple candidates are a single random selection, not a sequential retry, progressive-switch list or failure fallback; this is not device-verified",
			},
			newAdapter: control.NewSwitchThemeAdapter,
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
		ref := spec.newAdapter().Descriptor()
		out = append(out, adapterCapability{
			Name: ref.Name, Version: ref.Version, CoreType: spec.coreType, Input: spec.input,
			Acceptance: "local preparation and result interpretation only; authorized device acceptance: " + deviceAcceptance,
		})
	}
	return out
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
