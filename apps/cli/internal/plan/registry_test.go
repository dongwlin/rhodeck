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

// TestInputOnlyCapabilityMakesNoResultInterpretationClaim keeps a purely
// input-only capability from claiming interpreted results. The all-none
// branch also owns the acceptance text, and it takes precedence over
// InputOnlyNote instead of repeating the same boundary twice.
func TestInputOnlyCapabilityMakesNoResultInterpretationClaim(t *testing.T) {
	row := Spec{
		CoreType:          "FutureTask",
		ObjectivePolicies: []control.ObjectivePolicy{control.ObjectiveNone},
		InputOnlyNote:     "fixture inputs interpret no results",
		newAdapter:        func() control.TaskAdapter { return inputOnlyCapabilityAdapter{} },
	}.Row()
	require.Equal(t, "none", row.ResultInterpretation, "input-only adapter claims interpreted results: %+v", row)
	require.Equal(t, "local preparation only; result interpretation: none", row.Acceptance,
		"input-only acceptance boundary: %+v", row)
}
