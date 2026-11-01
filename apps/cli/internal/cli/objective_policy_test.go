package cli

import (
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
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
	if len(got.ObjectivePolicies) != 1 || got.ObjectivePolicies[0] != control.ObjectiveNone ||
		got.ResultInterpretation != "none" ||
		!strings.Contains(got.Acceptance, "local preparation only") ||
		!strings.Contains(got.Acceptance, "device acceptance: none") ||
		strings.Contains(got.Acceptance, "preparation and result interpretation") {
		t.Fatalf("input-only capability claims interpreted results: %+v", got)
	}
}
