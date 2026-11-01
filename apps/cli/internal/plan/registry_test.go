package plan

import (
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/stretchr/testify/require"
)

type inputOnlyCapabilityAdapter struct{}

func (inputOnlyCapabilityAdapter) Descriptor() control.AdapterRef {
	return control.AdapterRef{Name: "input-only-fixture", Version: 1}
}

func (inputOnlyCapabilityAdapter) Prepare(task control.Task) (control.PreparedTask, error) {
	return control.PreparedTask{Params: task.Params, Policy: control.ObjectiveNone}, nil
}

// TestInputOnlyCapabilityMakesNoResultInterpretationClaim keeps an input-only
// adapter from claiming interpreted results.
func TestInputOnlyCapabilityMakesNoResultInterpretationClaim(t *testing.T) {
	got := Spec{
		CoreType:          "FutureTask",
		ObjectivePolicies: []control.ObjectivePolicy{control.ObjectiveNone},
		newAdapter:        func() control.TaskAdapter { return inputOnlyCapabilityAdapter{} },
	}.Row()
	require.Len(t, got.ObjectivePolicies, 1, "input-only capability claims interpreted results: %+v", got)
	require.Equal(t, control.ObjectiveNone, got.ObjectivePolicies[0], "input-only capability claims interpreted results: %+v", got)
	require.Equal(t, "none", got.ResultInterpretation, "input-only capability claims interpreted results: %+v", got)
	require.Contains(t, got.Acceptance, "local preparation only", "input-only capability claims interpreted results: %+v", got)
	require.Contains(t, got.Acceptance, "device acceptance: none", "input-only capability claims interpreted results: %+v", got)
	require.NotContains(t, got.Acceptance, "preparation and result interpretation", "input-only capability claims interpreted results: %+v", got)
}

// TestAdapterRegistryFeedsEveryCommandSurface pins the four bundled adapters:
// the specs, the runtime registry and the help summary all carry the same
// Descriptor identities, so no command surface can drift from the registry.
func TestAdapterRegistryFeedsEveryCommandSurface(t *testing.T) {
	want := []control.AdapterRef{
		{Name: "oper_progress", Version: 1},
		{Name: "switch_theme", Version: 1},
		{Name: "copilot", Version: 1},
		{Name: "roguelike", Version: 1},
	}
	specs := specs()
	require.Len(t, specs, len(want), "registered %d adapter specs, want %d", len(specs), len(want))
	for i, spec := range specs {
		require.Equal(t, want[i], spec.newAdapter().Descriptor(), "spec %d descriptor", i)
	}
	adapters := Adapters()
	require.Len(t, adapters, len(want), "registry has %d adapters, want %d", len(adapters), len(want))
	for i, adapter := range adapters {
		require.Equal(t, want[i], adapter.Descriptor(), "registry adapter %d", i)
	}
	require.Equal(t, "oper_progress@1 (OperProgress), switch_theme@1 (SwitchTheme), copilot@1 (Copilot), roguelike@1 (Roguelike)",
		Summary(), "help summary")
	// The capability rows are derived from the same descriptors, so no command
	// surface can drift from the registry.
	rows := Rows()
	require.Len(t, rows, len(want), "capability rows = %d, registered adapters = %d", len(rows), len(want))
	for i, row := range rows {
		require.Equal(t, want[i].Name, row.Name, "capability row %d", i)
		require.Equal(t, want[i].Version, row.Version, "capability row %d", i)
	}
}
