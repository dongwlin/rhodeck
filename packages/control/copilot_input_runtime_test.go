package control_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/copilot"
)

func TestRuntimeCopilotInputOnlyUsesChainFacts(t *testing.T) {
	for _, test := range []struct {
		mode  string
		state State
	}{{"success", Succeeded}, {"failure", Failed}, {"stop", Stopped}} {
		for _, loop := range []string{"", `,"loop_times":2`} {
			t.Run(test.mode+loop, func(t *testing.T) {
				cfg := runtimeConfig(t, test.mode)
				cfg.Adapters = []TaskAdapter{copilot.New()}
				rt := newRuntime(t, cfg)
				plan := runtimePlan(t)
				ref := copilot.New().Descriptor()
				plan.Tasks[0] = Task{ID: "battle", Type: "Copilot", Adapter: &ref, Params: json.RawMessage(`{"filename":"9007199254740993.json"` + loop + `}`)}
				run, err := rt.Run(context.Background(), plan)
				require.NoError(t, err)
				if test.mode == "stop" {
					waitStarted(t, run)
					err := run.Stop(context.Background())
					require.NoError(t, err)
				}
				events, result := waitRuntimeRun(t, run)
				require.False(t, result.State != test.state || result.Cleanup != "complete" || result.Tasks[0].Policy != ObjectiveNone || result.Tasks[0].Typed != nil, "input-only chain result = %+v", result)
				for _, event := range events {
					require.False(t, event.Type == "typed" || event.Typed != nil, "input-only task emitted business facts: %+v", event)
				}
				record, err := ReadRecord(cfg.DataDir, result.RunID)
				require.False(t, err != nil || record.Result.State != test.state || record.Result.Tasks[0].Typed != nil || record.Result.Tasks[0].Policy != ObjectiveNone, "input-only record = %+v, %v", record, err)
			})
		}
	}
}
