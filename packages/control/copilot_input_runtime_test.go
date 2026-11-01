package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRuntimeCopilotInputOnlyUsesChainFacts(t *testing.T) {
	for _, test := range []struct {
		mode  string
		state State
	}{{"success", Succeeded}, {"failure", Failed}, {"stop", Stopped}} {
		for _, loop := range []string{"", `,"loop_times":2`} {
			t.Run(test.mode+loop, func(t *testing.T) {
				cfg := runtimeConfig(t, test.mode)
				cfg.Adapters = []TaskAdapter{NewCopilotAdapter()}
				rt := newRuntime(t, cfg)
				plan := runtimePlan(t)
				ref := NewCopilotAdapter().Descriptor()
				plan.Tasks[0] = Task{ID: "battle", Type: "Copilot", Adapter: &ref, Params: json.RawMessage(`{"filename":"9007199254740993.json"` + loop + `}`)}
				run, err := rt.Run(context.Background(), plan)
				if err != nil {
					t.Fatal(err)
				}
				if test.mode == "stop" {
					waitStarted(t, run)
					if err := run.Stop(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				events, result := waitRuntimeRun(t, run)
				if result.State != test.state || result.Cleanup != "complete" || result.Tasks[0].Policy != ObjectiveNone || result.Tasks[0].Typed != nil {
					t.Fatalf("input-only chain result = %+v", result)
				}
				for _, event := range events {
					if event.Type == "typed" || event.Typed != nil {
						t.Fatalf("input-only task emitted business facts: %+v", event)
					}
				}
				record, err := ReadRecord(cfg.DataDir, result.RunID)
				if err != nil || record.Result.State != test.state || record.Result.Tasks[0].Typed != nil || record.Result.Tasks[0].Policy != ObjectiveNone {
					t.Fatalf("input-only record = %+v, %v", record, err)
				}
			})
		}
	}
}
