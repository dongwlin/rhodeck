package app

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

func TestInputOnlyCapabilityMakesNoResultInterpretationClaim(t *testing.T) {
	got := cliAdapterCapability(cliAdapterSpec{
		coreType:          "FutureTask",
		objectivePolicies: []control.ObjectivePolicy{control.ObjectiveNone},
		newAdapter:        func() control.TaskAdapter { return inputOnlyCapabilityAdapter{} },
	})
	require.Len(t, got.ObjectivePolicies, 1, "input-only capability claims interpreted results: %+v", got)
	require.Equal(t, control.ObjectiveNone, got.ObjectivePolicies[0], "input-only capability claims interpreted results: %+v", got)
	require.Equal(t, "none", got.ResultInterpretation, "input-only capability claims interpreted results: %+v", got)
	require.Contains(t, got.Acceptance, "local preparation only", "input-only capability claims interpreted results: %+v", got)
	require.Contains(t, got.Acceptance, "device acceptance: none", "input-only capability claims interpreted results: %+v", got)
	require.NotContains(t, got.Acceptance, "preparation and result interpretation", "input-only capability claims interpreted results: %+v", got)
}
