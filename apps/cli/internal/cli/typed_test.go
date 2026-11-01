package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/stretchr/testify/require"
)

func adapterSelection(name string, version int) *control.AdapterRef {
	return &control.AdapterRef{Name: name, Version: version}
}

func strictTask(id, coreType, adapterName, params string) control.Task {
	return control.Task{ID: id, Type: coreType, Params: json.RawMessage(params), Adapter: adapterSelection(adapterName, 1)}
}

func operProgressTask(id, params string) control.Task {
	return strictTask(id, "OperProgress", "oper_progress", params)
}

func switchThemeTask(id, params string) control.Task {
	return strictTask(id, "SwitchTheme", "switch_theme", params)
}

func copilotTask(id, params string) control.Task {
	return strictTask(id, "Copilot", "copilot", params)
}

func roguelikeTask(id, params string) control.Task {
	return strictTask(id, "Roguelike", "roguelike", params)
}

func rawTask(id, coreType, params string) control.Task {
	return control.Task{ID: id, Type: coreType, Params: json.RawMessage(params)}
}

func typedPlan(t *testing.T, tasks ...control.Task) (control.Plan, string, string) {
	t.Helper()
	plan, executable, dir := testPlan(t)
	plan.Tasks = tasks
	return plan, executable, dir
}

func planFile(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	return path
}

func withInput(args []string, input string) []string {
	out := append([]string(nil), args...)
	out[2] = input
	return out
}

func runMachine(t *testing.T, args []string, input []byte) (int, string, []json.RawMessage, string) {
	t.Helper()
	var out, diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), args, bytes.NewReader(input), &out, &diagnostics)
	return code, out.String(), decodeLines(t, out.Bytes()), diagnostics.String()
}

func taskResult(t *testing.T, tasks []control.TaskResult, id string) control.TaskResult {
	t.Helper()
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	require.FailNow(t, "task %q missing from result", id)
	return control.TaskResult{}
}

func targetIDs(result *control.TypedResult) []string {
	if result == nil {
		return nil
	}
	ids := make([]string, len(result.Targets))
	for i, target := range result.Targets {
		ids[i] = target.ID
	}
	return ids
}

func TestCapabilitiesDescribeAdaptersWithoutClaimingDeviceAcceptance(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), []string{"capabilities"}, strings.NewReader(""), &out, &diagnostics)
	require.Equal(t, exitSuccess, code, "diagnostics: %s", diagnostics.String())
	var caps capabilities
	require.NoError(t, json.Unmarshal(out.Bytes(), &caps))
	adapters := cliAdapters()
	require.Len(t, caps.TaskAdapters, len(adapters), "adapter capabilities = %d, registered adapters = %d", len(caps.TaskAdapters), len(adapters))
	wantCore := map[string]string{
		"oper_progress": "OperProgress", "switch_theme": "SwitchTheme",
		"copilot": "Copilot", "roguelike": "Roguelike",
	}
	wantInput := map[string][]string{
		"oper_progress": {"Pioneer", "1-256", "skill_mastery"},
		"switch_theme":  {"1-256", "random", "retry"},
		"copilot":       {"copilot_list", "filename", "formation", "support_unit_usage", "loop_times"},
		"roguelike":     {"theme", "monthly_squad_auto_iterate", "deep_exploration_auto_iterate", "BlackFlow"},
	}
	// The fixed all, any, none order; each adapter lists exactly the policies
	// its inputs can select, never a single static value.
	wantPolicies := map[string][]control.ObjectivePolicy{
		"oper_progress": {control.ObjectiveAll},
		"switch_theme":  {control.ObjectiveAll},
		"copilot":       {control.ObjectiveAll, control.ObjectiveNone},
		"roguelike":     {control.ObjectiveAll, control.ObjectiveAny, control.ObjectiveNone},
	}
	// result_interpretation names the highest capability, not every input's.
	wantInterpretation := map[string]string{
		"oper_progress": "target_results", "switch_theme": "target_results",
		"copilot": "stage_results", "roguelike": "mode_objective",
	}
	wantSelection := map[string][]string{
		"oper_progress": {"all inputs use policy all"},
		"switch_theme":  {"all inputs use policy all"},
		"copilot":       {"copilot_list", "all", "filename", "loop_times", "none"},
		"roguelike":     {"starts_count", "monthly_squad_auto_iterate", "deep_exploration_auto_iterate", "none"},
	}
	for i, adapter := range adapters {
		ref := adapter.Descriptor()
		got := caps.TaskAdapters[i]
		require.Equal(t, ref.Name, got.Name, "capability %d = %+v, descriptor = %+v", i, got, ref)
		require.Equal(t, ref.Version, got.Version, "capability %d = %+v, descriptor = %+v", i, got, ref)
		require.Equal(t, wantCore[got.Name], got.CoreType, "capability %d lacks core type: %+v", i, got)
		require.Equal(t, wantPolicies[got.Name], got.ObjectivePolicies, "capability %d objective policies", i)
		require.Equal(t, wantInterpretation[got.Name], got.ResultInterpretation, "capability %d interpretation", i)
		for _, fragment := range wantInput[got.Name] {
			require.Contains(t, got.Input, fragment, "capability %d input lacks %q: %q", i, fragment, got.Input)
		}
		for _, fragment := range wantSelection[got.Name] {
			require.Contains(t, got.PolicySelection, fragment, "capability %d policy selection lacks %q: %q", i, fragment, got.PolicySelection)
		}
		require.Contains(t, got.Acceptance, "device acceptance: none", "capability %d claims acceptance: %q", i, got.Acceptance)
	}
	// Input-only branches are named as none inside the descriptive fields; the
	// capability report keeps no single static objective_policy field.
	require.NotContains(t, out.String(), `"objective_policy"`, "capabilities kept a single static objective_policy field: %s", out.String())
	require.Contains(t, caps.TaskAdapters[2].Acceptance, "does not prove a clear or a safe device handoff", "copilot acceptance lacks the single-file caveat")
	require.Contains(t, caps.TaskAdapters[3].Acceptance, "modes without a reliable terminal fact", "roguelike acceptance lacks the input-only modes")
	require.Contains(t, caps.AdapterBasis, "9b75f9f7", "adapter basis: %q", caps.AdapterBasis)
	require.Contains(t, caps.AdapterBasis, "not executed", "adapter basis: %q", caps.AdapterBasis)
	require.NotContains(t, caps.AdapterBasis, "v6.20.1", "source review conflates the release candidate: %q", caps.AdapterBasis)
	for _, fragment := range []string{"rolling", "caller-provided", "no fixed Core version", "old-version compatibility", "doctor/run/record"} {
		require.Contains(t, caps.CoreVersionPolicy, fragment, "Core version policy lacks %q: %q", fragment, caps.CoreVersionPolicy)
	}
	require.NotContains(t, out.String(), `"static_snapshot_basis"`, "capabilities retains a fixed release snapshot: %s", out.String())
	require.NotContains(t, out.String(), "v6.20.1", "capabilities retains a fixed release snapshot: %s", out.String())
	require.Contains(t, caps.InstallationFacts, "not inspected", "installation facts: %q", caps.InstallationFacts)
	require.Equal(t, "none", caps.DeviceAcceptance, "device acceptance: %q", caps.DeviceAcceptance)
	require.False(t, caps.NativeTaskDiscovery, "generic capabilities changed: %+v", caps)
	require.NotEmpty(t, caps.TaskInput, "generic capabilities changed: %+v", caps)
	require.NotEmpty(t, caps.TaskAcceptance, "generic capabilities changed: %+v", caps)

	out.Reset()
	diagnostics.Reset()
	code = runCLI("dev", context.Background(), []string{"help"}, strings.NewReader(""), &out, &diagnostics)
	require.Equal(t, exitSuccess, code, "help exit")
	var help envelope
	require.NoError(t, json.Unmarshal(out.Bytes(), &help))
	for _, name := range []string{"oper_progress@1", "switch_theme@1", "copilot@1", "roguelike@1", "task.adapter"} {
		require.Contains(t, help.Usage, name, "help does not mention %q: %s", name, help.Usage)
	}
	require.Contains(t, help.Usage, "actual objective policy", "help does not state the per-task policy preview: %s", help.Usage)
	require.NotContains(t, help.Usage, "Both adapters", "help does not state the per-task policy preview: %s", help.Usage)
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
	specs := cliAdapterSpecs()
	require.Len(t, specs, len(want), "registered %d adapter specs, want %d", len(specs), len(want))
	for i, spec := range specs {
		require.Equal(t, want[i], spec.newAdapter().Descriptor(), "spec %d descriptor", i)
	}
	adapters := cliAdapters()
	require.Len(t, adapters, len(want), "registry has %d adapters, want %d", len(adapters), len(want))
	for i, adapter := range adapters {
		require.Equal(t, want[i], adapter.Descriptor(), "registry adapter %d", i)
	}
	require.Equal(t, "oper_progress@1 (OperProgress), switch_theme@1 (SwitchTheme), copilot@1 (Copilot), roguelike@1 (Roguelike)",
		cliAdapterSummary(), "help summary")
}

// TestCopilotDryRunSeparatesListPoliciesFromInputOnlySingleFile verifies that
// the same adapter reports policy all with per-entry stage targets for a
// copilot_list and policy none without a typed result for a single file with
// loops, entirely without side effects.
func TestCopilotDryRunSeparatesListPoliciesFromInputOnlySingleFile(t *testing.T) {
	plan, _, dir := typedPlan(t,
		copilotTask("copilot-list", `{"copilot_list":[{"filename":"job.json"},{"filename":"job.json","is_raid":true}]}`),
		copilotTask("copilot-single", `{"filename":"job.json","loop_times":3}`),
	)
	input := encodePlan(t, plan)
	missingWorker := filepath.Join(t.TempDir(), "missing-worker")
	code, text, lines, diagnostics := runMachine(t, cliArgs("dry-run", missingWorker, dir), input)
	require.Equal(t, exitSuccess, code, "%s (%s)", text, diagnostics)
	require.Len(t, lines, 1, "dry-run wrote %d records", len(lines))
	var preview envelope
	require.NoError(t, json.Unmarshal(lines[0], &preview), "missing compiled preview: %s", text)
	require.Equal(t, "plan", preview.Type, "missing compiled preview: %s", text)
	require.NotNil(t, preview.Plan, "missing compiled preview: %s", text)
	require.Len(t, preview.Tasks, 2, "missing compiled preview: %s", text)
	list := preview.Tasks[0]
	require.Equal(t, control.ObjectiveAll, list.Policy, "copilot list policy: %+v", list)
	require.NotNil(t, list.Typed, "copilot list policy: %+v", list)
	require.Equal(t, list.Policy, list.Typed.Policy, "copilot list policy: %+v", list)
	require.Equal(t, []string{"copilot:0", "copilot:1"}, targetIDs(list.Typed), "copilot list targets")
	for _, target := range list.Typed.Targets {
		require.Equal(t, control.TargetUnobserved, target.Outcome, "copilot list preview claims a fact: %+v", target)
		require.Empty(t, target.Code, "copilot list preview claims a fact: %+v", target)
		require.Empty(t, target.Details, "copilot list preview claims a fact: %+v", target)
	}
	single := preview.Tasks[1]
	require.Equal(t, control.ObjectiveNone, single.Policy, "single-file copilot task: %+v", single)
	require.Nil(t, single.Typed, "single-file copilot task: %+v", single)
	got := string(preview.Plan.Tasks[0].Params)
	require.Contains(t, got, `"id":0`, "compiled list lost explicit ids or raid flag: %s", got)
	require.Contains(t, got, `"id":1`, "compiled list lost explicit ids or raid flag: %s", got)
	require.Contains(t, got, `"is_raid":true`, "compiled list lost explicit ids or raid flag: %s", got)
	got = string(preview.Plan.Tasks[1].Params)
	require.Contains(t, got, `"loop_times":3`, "compiled single-file input changed: %s", got)
	require.NotContains(t, got, "copilot_list", "compiled single-file input changed: %s", got)
	require.Contains(t, text, `"objective_policy":"all"`, "dry-run did not report both per-task policies: %s", text)
	require.Contains(t, text, `"objective_policy":"none"`, "dry-run did not report both per-task policies: %s", text)
	joined := strings.Join(preview.Unverified, "\n")
	require.Contains(t, joined, "copilot:", "copilot caveats missing: %v", preview.Unverified)
	for _, fragment := range []string{"never inspected during preparation", "no authorized device acceptance", "does not prove a clear or a safe device handoff"} {
		require.Contains(t, joined, fragment, "copilot caveats lack %q: %v", fragment, preview.Unverified)
	}
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "dry-run created the data directory")
}

// TestRoguelikeDryRunSelectsPoliciesByModeAndSwitches verifies that the
// roguelike adapter previews every policy from the compiled input: modes 6 and
// 7 stay none with the rotation switch omitted or false, become all when the
// switch is true, and mode 0 selects all for one declared run and any for
// several.
func TestRoguelikeDryRunSelectsPoliciesByModeAndSwitches(t *testing.T) {
	plan, _, dir := typedPlan(t,
		roguelikeTask("squad-omitted", `{"theme":"Sami","mode":6}`),
		roguelikeTask("squad-false", `{"theme":"Sami","mode":6,"monthly_squad_auto_iterate":false}`),
		roguelikeTask("squad-true", `{"theme":"Sami","mode":6,"monthly_squad_auto_iterate":true}`),
		roguelikeTask("explore-omitted", `{"theme":"Mizuki","mode":7}`),
		roguelikeTask("explore-true", `{"theme":"Mizuki","mode":7,"deep_exploration_auto_iterate":true}`),
		roguelikeTask("runs-single", `{"theme":"Phantom","mode":0,"starts_count":1}`),
		roguelikeTask("runs-several", `{"theme":"Phantom","mode":0,"starts_count":3}`),
	)
	input := encodePlan(t, plan)
	missingWorker := filepath.Join(t.TempDir(), "missing-worker")
	code, text, lines, diagnostics := runMachine(t, cliArgs("dry-run", missingWorker, dir), input)
	require.Equal(t, exitSuccess, code, "%s (%s)", text, diagnostics)
	var preview envelope
	require.NoError(t, json.Unmarshal(lines[0], &preview), "missing compiled preview: %s", text)
	require.Equal(t, "plan", preview.Type, "missing compiled preview: %s", text)
	require.Len(t, preview.Tasks, 7, "missing compiled preview: %s", text)
	for i, want := range []struct {
		policy control.ObjectivePolicy
		target string
	}{
		{control.ObjectiveNone, ""}, {control.ObjectiveNone, ""},
		{control.ObjectiveAll, "roguelike:monthly_squad"}, {control.ObjectiveNone, ""},
		{control.ObjectiveAll, "roguelike:deep_exploration"},
		{control.ObjectiveAll, "roguelike:run:1"},
	} {
		task := preview.Tasks[i]
		require.Equal(t, want.policy, task.Policy, "task %d policy", i)
		if want.policy == control.ObjectiveNone {
			require.Nil(t, task.Typed, "input-only task %d gained a typed preview: %+v", i, task.Typed)
			continue
		}
		require.NotNil(t, task.Typed, "task %d lacks its typed preview: %+v", i, task)
		require.Equal(t, task.Policy, task.Typed.Policy, "task %d lacks its typed preview: %+v", i, task)
		require.Equal(t, []string{want.target}, targetIDs(task.Typed), "task %d targets", i)
	}
	require.Equal(t, []string{"roguelike:run:1", "roguelike:run:2", "roguelike:run:3"}, targetIDs(preview.Tasks[6].Typed), "multi-run targets")
	require.Equal(t, control.ObjectiveAny, preview.Tasks[6].Typed.Policy, "multi-run policy")
	joined := strings.Join(preview.Unverified, "\n")
	require.Contains(t, joined, "roguelike:", "roguelike caveats missing: %v", preview.Unverified)
	require.Contains(t, joined, "no authorized device acceptance", "roguelike caveats missing: %v", preview.Unverified)
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "dry-run created the data directory")
}

func TestTypedDryRunPreviewsCompiledTargetsWithoutEffects(t *testing.T) {
	plan, _, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"name":"Alpha","role":"Warrior","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`),
		switchThemeTask("theme-1", `{"themes":["Theme One","Theme Two"]}`),
		rawTask("raw-1", "FutureTask", `{"future":900719925474099312345}`),
	)
	input := encodePlan(t, plan)
	file := planFile(t, input)
	missingWorker := filepath.Join(t.TempDir(), "missing-worker")
	sources := map[string]string{"stdin": "-", "file": file}

	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "commands")
			t.Setenv("RHODECK_CLI_TRACE", trace)
			code, text, lines, diagnostics := runMachine(t, withInput(cliArgs("dry-run", missingWorker, dir), source), input)
			require.Equal(t, exitSuccess, code, "%s", diagnostics)
			require.Len(t, lines, 1, "dry-run wrote %d records", len(lines))
			var preview envelope
			require.NoError(t, json.Unmarshal(lines[0], &preview))
			require.Equal(t, "plan", preview.Type, "missing compiled preview: %s", text)
			require.NotNil(t, preview.Plan, "missing compiled preview: %s", text)
			require.Len(t, preview.Tasks, 3, "missing compiled preview: %s", text)
			require.Equal(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`,
				string(preview.Plan.Tasks[0].Params), "compiled plans")
			require.Equal(t, `{"themes":["Theme One","Theme Two"]}`, string(preview.Plan.Tasks[1].Params), "compiled themes")
			require.Equal(t, `{"future":900719925474099312345}`, string(preview.Plan.Tasks[2].Params), "raw params changed")
			require.Equal(t, []string{"op-0001", "op-0002"}, targetIDs(preview.Tasks[0].Typed), "operator targets")
			require.Equal(t, []string{"theme-selection"}, targetIDs(preview.Tasks[1].Typed), "theme targets")
			for _, task := range preview.Tasks[:2] {
				require.Equal(t, control.ObjectiveAll, task.Policy, "prepared task lacks an unobserved aggregate: %+v", task)
				require.NotNil(t, task.Typed, "prepared task lacks an unobserved aggregate: %+v", task)
				require.Equal(t, task.Policy, task.Typed.Policy, "prepared task lacks an unobserved aggregate: %+v", task)
				require.Equal(t, control.Unknown, task.Typed.State, "prepared task lacks an unobserved aggregate: %+v", task)
				require.Equal(t, "typed_incomplete", task.Typed.Code, "prepared task lacks an unobserved aggregate: %+v", task)
				for _, target := range task.Typed.Targets {
					require.Equal(t, control.TargetUnobserved, target.Outcome, "preview claims a fact: %+v", target)
					require.Empty(t, target.Code, "preview claims a fact: %+v", target)
					require.Empty(t, target.Details, "preview claims a fact: %+v", target)
				}
			}
			require.Nil(t, preview.Tasks[2].Typed, "raw task gained a typed result: %+v", preview.Tasks[2].Typed)
			joined := strings.Join(preview.Unverified, "\n")
			require.Contains(t, joined, "oper_progress:", "adapter caveats missing: %v", preview.Unverified)
			require.Contains(t, joined, "switch_theme:", "adapter caveats missing: %v", preview.Unverified)
			// The preview must name every condition strict local preparation
			// cannot verify, not just a sample of them.
			for _, fragment := range []string{
				"operator identity", "role agreement", "skill existence", "elite prerequisites",
				"materials", "inventory", "training-room state",
				"training at the target level started", "does not prove training finished",
				"theme availability", "unlock state", "final theme state",
				"single random selection", "not a sequential retry", "failure fallback",
				"device-verified",
			} {
				require.Contains(t, joined, fragment, "preview unverified conditions lack %q: %v", fragment, preview.Unverified)
			}
			_, err := os.Stat(dir)
			require.ErrorIs(t, err, os.ErrNotExist, "dry-run created the data directory")
			_, err = os.Stat(trace)
			require.ErrorIs(t, err, os.ErrNotExist, "dry-run launched a worker")
		})
	}

	// run --dry-run is the same side-effect-free preview.
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	args := append(cliArgs("run", missingWorker, dir), "--dry-run")
	code, text, lines, diagnostics := runMachine(t, args, input)
	require.Equal(t, exitSuccess, code, "run --dry-run: %s (%s)", text, diagnostics)
	require.Len(t, lines, 1, "run --dry-run: %s (%s)", text, diagnostics)
	var preview envelope
	require.NoError(t, json.Unmarshal(lines[0], &preview), "run --dry-run did not preview: %s", text)
	require.Equal(t, "plan", preview.Type, "run --dry-run did not preview: %s", text)
	require.NotNil(t, preview.Plan, "run --dry-run did not preview: %s", text)
	_, err := os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist, "run --dry-run created the data directory")
	_, err = os.Stat(trace)
	require.ErrorIs(t, err, os.ErrNotExist, "run --dry-run launched a worker")
}

func readTaskTrace(t *testing.T, path string) [][2]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	out := make([][2]string, len(lines))
	for i, line := range lines {
		fields := strings.SplitN(line, "\t", 2)
		require.Len(t, fields, 2, "malformed task trace line: %q", line)
		out[i] = [2]string{fields[0], fields[1]}
	}
	return out
}

func TestTypedRunFileAndStdinReachTheSameTerminalFacts(t *testing.T) {
	for _, useFile := range []bool{false, true} {
		name := "stdin"
		if useFile {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
			taskTrace := filepath.Join(t.TempDir(), "submitted")
			t.Setenv("RHODECK_CLI_TASK_TRACE", taskTrace)
			plan, executable, dir := typedPlan(t,
				operProgressTask("grow-1", `{"plans":[{"name":"Alpha","role":"Warrior","elite":2}]}`),
				switchThemeTask("theme-1", `{"themes":["Theme One","Theme Two"]}`),
				rawTask("raw-1", "FutureTask", `{"future":900719925474099312345}`),
			)
			input := encodePlan(t, plan)
			source := "-"
			if useFile {
				source = planFile(t, input)
			}

			// The preview names the same targets the run later reports.
			code, text, lines, diagnostics := runMachine(t, withInput(cliArgs("dry-run", executable, dir), source), input)
			require.Equal(t, exitSuccess, code, "preview: %s (%s)", text, diagnostics)
			require.Len(t, lines, 1, "preview: %s (%s)", text, diagnostics)
			var preview envelope
			require.NoError(t, json.Unmarshal(lines[0], &preview), "bad preview: %s", text)
			require.NotNil(t, preview.Plan, "bad preview: %s", text)
			require.Len(t, preview.Tasks, 3, "bad preview: %s", text)

			code, text, lines, diagnostics = runMachine(t, withInput(cliArgs("run", executable, dir), source), input)
			require.Equal(t, exitSuccess, code, "diagnostics: %s (%s)", diagnostics, text)
			var terminal envelope
			require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal record: %s", text)
			require.Equal(t, "terminal", terminal.Type, "missing terminal record: %s", text)
			require.NotNil(t, terminal.Result, "missing terminal record: %s", text)
			result := terminal.Result
			require.Equal(t, control.Succeeded, result.State, "result: %+v", result)
			require.Empty(t, result.Code, "result: %+v", result)
			require.Equal(t, "complete", result.Cleanup, "result: %+v", result)
			grow := taskResult(t, result.Tasks, "grow-1")
			require.Equal(t, control.Succeeded, grow.State, "operator task: %+v", grow)
			require.NotNil(t, grow.Typed, "operator task: %+v", grow)
			require.Equal(t, control.Succeeded, grow.Typed.State, "operator task: %+v", grow)
			require.Equal(t, targetIDs(preview.Tasks[0].Typed), targetIDs(grow.Typed), "operator preview targets %v, terminal targets %v", targetIDs(preview.Tasks[0].Typed), targetIDs(grow.Typed))
			require.Equal(t, "op-0001", grow.Typed.Targets[0].ID, "operator fact: %+v", grow.Typed.Targets[0])
			require.Equal(t, control.TargetAchieved, grow.Typed.Targets[0].Outcome, "operator fact: %+v", grow.Typed.Targets[0])
			require.Equal(t, "completed", grow.Typed.Targets[0].Code, "operator fact: %+v", grow.Typed.Targets[0])
			theme := taskResult(t, result.Tasks, "theme-1")
			require.Equal(t, control.Succeeded, theme.State, "theme task: %+v", theme)
			require.NotNil(t, theme.Typed, "theme task: %+v", theme)
			require.Equal(t, control.Succeeded, theme.Typed.State, "theme task: %+v", theme)
			require.Equal(t, targetIDs(preview.Tasks[1].Typed), targetIDs(theme.Typed), "theme preview targets %v, terminal targets %v", targetIDs(preview.Tasks[1].Typed), targetIDs(theme.Typed))
			require.Equal(t, "theme-selection", theme.Typed.Targets[0].ID, "theme fact: %+v", theme.Typed.Targets[0])
			require.Equal(t, control.TargetAlreadySet, theme.Typed.Targets[0].Outcome, "theme fact: %+v", theme.Typed.Targets[0])
			require.Equal(t, "already_set", theme.Typed.Targets[0].Code, "theme fact: %+v", theme.Typed.Targets[0])
			raw := taskResult(t, result.Tasks, "raw-1")
			require.Equal(t, control.Succeeded, raw.State, "raw task gained typed facts: %+v", raw)
			require.Nil(t, raw.Typed, "raw task gained typed facts: %+v", raw)

			// The worker received exactly the types and compiled parameters that
			// dry-run previewed; raw parameters were not rewritten.
			submitted := readTaskTrace(t, taskTrace)
			require.Len(t, submitted, len(preview.Plan.Tasks), "worker received %d tasks, preview has %d", len(submitted), len(preview.Plan.Tasks))
			for i, got := range submitted {
				want := preview.Plan.Tasks[i]
				require.Equal(t, want.Type, got[0], "task %d submitted %q %s, preview %q %s", i, got[0], got[1], want.Type, want.Params)
				require.Equal(t, string(want.Params), got[1], "task %d submitted %q %s, preview %q %s", i, got[0], got[1], want.Type, want.Params)
			}

			typedEvents := 0
			latest := make(map[string]*control.TypedResult)
			for _, line := range lines {
				var event struct {
					Type   string               `json:"type"`
					TaskID string               `json:"task_id"`
					Typed  *control.TypedResult `json:"typed"`
					Detail json.RawMessage      `json:"details"`
				}
				require.NoError(t, json.Unmarshal(line, &event))
				if event.Type != "typed" {
					continue
				}
				typedEvents++
				require.NotNil(t, event.Typed, "typed event without a typed result or task: %s", line)
				require.NotEmpty(t, event.TaskID, "typed event without a typed result or task: %s", line)
				require.Empty(t, event.Detail, "typed event disclosed raw callback details: %s", line)
				latest[event.TaskID] = event.Typed
			}
			require.Equal(t, 3, typedEvents, "typed events = %d (want operator detail, operator summary, theme fact)", typedEvents)
			// The final typed event, terminal result and durable record carry the
			// same task result; no second business state is maintained.
			for id, want := range latest {
				require.Equal(t, want, taskResult(t, result.Tasks, id).Typed, "%s event and terminal typed result disagree", id)
			}

			record, err := control.ReadRecord(dir, result.RunID)
			require.NoError(t, err)
			require.Equal(t, result.Tasks, record.Result.Tasks, "terminal and record disagree: %+v vs %+v", record.Result, result)
			require.Equal(t, result.State, record.Result.State, "terminal and record disagree: %+v vs %+v", record.Result, result)
			require.Equal(t, result.Cleanup, record.Result.Cleanup, "terminal and record disagree: %+v vs %+v", record.Result, result)
			require.Contains(t, string(record.Plan.Tasks[0].Params), `"name":"Alpha","role":"Warrior"`, "record did not keep the caller plan: %s", record.Plan.Tasks[0].Params)
			require.Contains(t, string(record.Plan.Tasks[2].Params), "900719925474099312345", "record lost the raw integer literal: %s", record.Plan.Tasks[2].Params)
		})
	}
}

func TestTypedBusinessFailureOverridesChainSuccess(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "failure")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","skill_level":7}]}`),
		switchThemeTask("theme-1", `{"themes":["Theme One"]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	require.Equal(t, exitFailed, code, "diagnostics: %s (%s)", diagnostics, text)
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", text)
	require.NotNil(t, terminal.Result, "missing terminal: %s", text)
	result := terminal.Result
	require.Equal(t, control.Failed, result.State, "result: %+v", result)
	require.Equal(t, "typed_failed", result.Code, "result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.Equal(t, control.Succeeded, grow.State, "typed failure rewrote the chain fact: %+v", grow)
	require.Equal(t, control.TargetFailed, grow.Typed.Targets[0].Outcome, "operator failure: %+v", grow.Typed.Targets[0])
	require.Equal(t, "resource_insufficient", grow.Typed.Targets[0].Code, "operator failure: %+v", grow.Typed.Targets[0])
	theme := taskResult(t, result.Tasks, "theme-1")
	require.Equal(t, control.Succeeded, theme.State, "theme result: %+v", theme)
	require.Equal(t, control.Failed, theme.Typed.State, "theme result: %+v", theme)
	require.Equal(t, control.TargetLocked, theme.Typed.Targets[0].Outcome, "theme fact: %+v", theme.Typed.Targets[0])
	require.Equal(t, "locked", theme.Typed.Targets[0].Code, "theme fact: %+v", theme.Typed.Targets[0])
}

func TestTypedFactsSurviveWorkerFaultAsUnknown(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "fault")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","skill_level":7}]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	require.Equal(t, exitUnknown, code, "diagnostics: %s (%s)", diagnostics, text)
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", text)
	require.NotNil(t, terminal.Result, "missing terminal: %s", text)
	result := terminal.Result
	require.Equal(t, control.Unknown, result.State, "fault result: %+v", result)
	require.Equal(t, "instance_fault", result.Code, "fault result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "fault result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.NotEqual(t, control.Succeeded, grow.State, "fault left a successful chain fact: %+v", grow)
	require.NotNil(t, grow.Typed, "confirmed typed fact was lost on fault: %+v", grow)
	require.Equal(t, control.Failed, grow.Typed.State, "confirmed typed fact was lost on fault: %+v", grow)
	require.Equal(t, control.TargetFailed, grow.Typed.Targets[0].Outcome, "typed target fact: %+v", grow.Typed.Targets[0])
	require.Equal(t, "resource_insufficient", grow.Typed.Targets[0].Code, "typed target fact: %+v", grow.Typed.Targets[0])
}

// markerCancelWriter cancels the run after the marker is written, so a test can
// stop the run after a specific observed fact.
type markerCancelWriter struct {
	bytes.Buffer
	cancel context.CancelFunc
	marker []byte
}

func (w *markerCancelWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, w.marker) {
		w.cancel()
	}
	return n, err
}

func TestConfirmedStopSurvivesCleanupFailure(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "stopcleanupfail")
	plan, executable, dir := typedPlan(t, rawTask("raw-1", "FutureTask", `{"future":900719925474099312345}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &markerCancelWriter{cancel: cancel, marker: []byte(`"state":"started"`)}
	var diagnostics bytes.Buffer
	code := runCLI("dev", ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	require.Equal(t, exitUnknown, code, "diagnostics: %s", diagnostics.String())
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", out.String())
	require.NotNil(t, terminal.Result, "missing terminal: %s", out.String())
	result := terminal.Result
	// A worker that reports a Close error and exits immediately can be observed
	// either through that error or through the following disconnect. Both are
	// cleanup failures, and neither may rewrite the confirmed stop.
	require.Equal(t, control.Stopped, result.State, "confirmed stop was rewritten by cleanup: %+v", result)
	require.Equal(t, "failed", result.Cleanup, "confirmed stop was rewritten by cleanup: %+v", result)
	require.True(t, result.CleanupCode == "close_failed" || result.CleanupCode == "disconnected",
		"confirmed stop was rewritten by cleanup: %+v", result)
}

func TestStopWithEstablishedTypedFailureSurvivesCleanupFailure(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "stoptypedcleanupfail")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","skill_level":7}]}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &markerCancelWriter{cancel: cancel, marker: []byte(`"type":"typed"`)}
	var diagnostics bytes.Buffer
	code := runCLI("dev", ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	require.Equal(t, exitUnknown, code, "diagnostics: %s", diagnostics.String())
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", out.String())
	require.NotNil(t, terminal.Result, "missing terminal: %s", out.String())
	result := terminal.Result
	// As above, the close error and the disconnect are transport-equivalent
	// cleanup failures; the established typed failure and its target fact must
	// survive either observation.
	require.Equal(t, control.Failed, result.State, "established failure was rewritten by cleanup: %+v", result)
	require.Equal(t, "typed_failed", result.Code, "established failure was rewritten by cleanup: %+v", result)
	require.Equal(t, "failed", result.Cleanup, "established failure was rewritten by cleanup: %+v", result)
	require.True(t, result.CleanupCode == "close_failed" || result.CleanupCode == "disconnected",
		"established failure was rewritten by cleanup: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.NotNil(t, grow.Typed, "typed failure was lost: %+v", grow)
	require.Equal(t, control.Failed, grow.Typed.State, "typed failure was lost: %+v", grow)
	require.Equal(t, control.TargetFailed, grow.Typed.Targets[0].Outcome, "typed failure was lost: %+v", grow)
}

func TestEstablishedFailureSurvivesFaultAndCleanupFailure(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "prooffault")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","skill_level":7}]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	require.Equal(t, exitUnknown, code, "diagnostics: %s (%s)", diagnostics, text)
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", text)
	require.NotNil(t, terminal.Result, "missing terminal: %s", text)
	result := terminal.Result
	require.Equal(t, control.Failed, result.State, "proof state was rewritten by fault cleanup: %+v", result)
	require.Equal(t, "typed_failed", result.Code, "proof state was rewritten by fault cleanup: %+v", result)
	require.Equal(t, "failed", result.Cleanup, "proof state was rewritten by fault cleanup: %+v", result)
	require.Equal(t, "instance_fault", result.CleanupCode, "proof state was rewritten by fault cleanup: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.Equal(t, control.Succeeded, grow.State, "chain and typed facts changed: %+v", grow)
	require.NotNil(t, grow.Typed, "chain and typed facts changed: %+v", grow)
	require.Equal(t, control.Failed, grow.Typed.State, "chain and typed facts changed: %+v", grow)
}

func TestRecordWriteFailuresUsePersistenceExitCode(t *testing.T) {
	for _, result := range []control.Result{
		{State: control.Rejected, Code: "record_write_failed", Cleanup: "complete"},
		{State: control.Succeeded, Cleanup: "complete", CleanupCode: "record_write_failed"},
	} {
		require.Equal(t, exitUnknown, resultExit(result), "resultExit: %+v", result)
	}
	require.Equal(t, exitRejected, resultExit(control.Result{State: control.Rejected, Code: "invalid_plan", Cleanup: "complete"}), "ordinary rejection exit")
	wrapped := fmt.Errorf("admission: %w", control.ErrRecordWrite)
	require.Equal(t, exitUnknown, errorExit(wrapped), "errorExit(ErrRecordWrite)")
	require.Equal(t, "record_write_failed", errorCode(wrapped), "errorCode(ErrRecordWrite)")
}

func TestAdmissionPersistenceFailureIsClassified(t *testing.T) {
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	rt, err := control.New(control.Config{DataDir: dir, Adapters: cliAdapters(), Worker: worker.Config{Executable: executable}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	runs := filepath.Join(dir, "runs")
	require.NoError(t, os.RemoveAll(runs))
	require.NoError(t, os.WriteFile(runs, []byte("blocked"), 0600))
	_, err = rt.Run(context.Background(), plan)
	require.ErrorIs(t, err, control.ErrRecordWrite, "admission error")
	require.Equal(t, "record_write_failed", errorCode(err), "admission error code")
	require.Equal(t, exitUnknown, errorExit(err), "admission exit")
}

// TestInitializePersistenceFailureUsesPersistenceExitCode drives the real child
// CLI and blocks the worker inside Initialize until the test has made the run
// directory unwritable. The post-initialize save then fails after admission and
// must surface as record_write_failed with exit 6 rather than an environment
// rejection. The final save may also fail, so the terminal state can be rejected
// or unknown; both keep the persistence code and exit.
func TestInitializePersistenceFailureUsesPersistenceExitCode(t *testing.T) {
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	release := filepath.Join(t.TempDir(), "release")
	trace := filepath.Join(t.TempDir(), "commands")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, cliArgs("run", executable, dir)...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(filteredCLIEnv(), "RHODECK_CLI_PROCESS=1", "RHODECK_CLI_WORKER_MODE=initslow",
		"RHODECK_CLI_INIT_RELEASE="+release, "RHODECK_CLI_TRACE="+trace)
	cmd.Stdin = bytes.NewReader(encodePlan(t, plan))
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stopRead := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stopRead()
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	scanner := bufio.NewScanner(stdout)
	var lines []json.RawMessage
	sabotaged := false
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		lines = append(lines, line)
		if sabotaged || !bytes.Contains(line, []byte(`"state":"admitted"`)) {
			continue
		}
		runs := filepath.Join(dir, "runs")
		require.NoError(t, os.RemoveAll(runs))
		require.NoError(t, os.WriteFile(runs, []byte("blocked"), 0600))
		require.NoError(t, os.WriteFile(release, nil, 0600))
		sabotaged = true
	}
	require.NoError(t, scanner.Err())
	waitErr := cmd.Wait()
	require.NoError(t, ctx.Err(), "CLI did not finish within its deadline")
	require.True(t, sabotaged, "CLI never reached admission: %s", diagnostics.String())
	require.NotNil(t, cmd.ProcessState, "CLI did not exit: %v diagnostics=%s", waitErr, diagnostics.String())
	require.Equal(t, exitUnknown, cmd.ProcessState.ExitCode(), "exit (%v): %s", waitErr, diagnostics.String())
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal record")
	require.NotNil(t, terminal.Result, "missing terminal record")
	// The post-initialize save fails first. While the run directory is still
	// blocked the terminal save fails too, so the final result may be
	// rejected or unknown; both must carry record_write_failed and exit 6.
	result := terminal.Result
	require.Equal(t, "record_write_failed", result.Code, "persistence failure result: %+v", result)
	require.Contains(t, []control.State{control.Rejected, control.Unknown}, result.State, "persistence failure result: %+v", result)
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	for _, forbidden := range []string{"connect", "append_task", "start"} {
		require.False(t, bytes.Contains(commands, []byte(forbidden+"\n")), "persistence failure issued %q: %s", forbidden, commands)
	}
	require.True(t, bytes.Contains(commands, []byte("initialize\n")), "worker was not initialized: %s", commands)
}

// filteredCLIEnv removes parent-process fixture switches so the child CLI and
// its worker observe exactly the values this test sets.
func filteredCLIEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		switch {
		case strings.HasPrefix(entry, "RHODECK_CLI_PROCESS="),
			strings.HasPrefix(entry, "RHODECK_CLI_WORKER_MODE="),
			strings.HasPrefix(entry, "RHODECK_CLI_INIT_RELEASE="),
			strings.HasPrefix(entry, "RHODECK_CLI_WORKER_PID="),
			strings.HasPrefix(entry, "RHODECK_CLI_TRACE="),
			strings.HasPrefix(entry, "RHODECK_CLI_TASK_TRACE="):
			continue
		default:
			env = append(env, entry)
		}
	}
	return env
}

func TestTypedIncompleteAndReportedObservationsStayUnknown(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		tasks    []control.Task
		outcomes map[string]control.TargetOutcome
	}{
		{
			mode: "incomplete",
			tasks: []control.Task{
				operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
				switchThemeTask("theme-1", `{"themes":["Theme One"]}`),
			},
			outcomes: map[string]control.TargetOutcome{"grow-1": control.TargetUnobserved, "theme-1": control.TargetUnobserved},
		},
		{
			mode: "reported",
			tasks: []control.Task{
				operProgressTask("grow-1", `{"plans":[{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`),
				switchThemeTask("theme-1", `{"themes":["Theme One"]}`),
			},
			outcomes: map[string]control.TargetOutcome{
				"grow-1":  control.TargetReportedPrerequisiteStarted,
				"theme-1": control.TargetConfirmationObserved,
			},
		},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("RHODECK_CLI_WORKER_MODE", tc.mode)
			plan, executable, dir := typedPlan(t, tc.tasks...)
			code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
			require.Equal(t, exitUnknown, code, "diagnostics: %s (%s)", diagnostics, text)
			var terminal envelope
			require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", text)
			require.NotNil(t, terminal.Result, "missing terminal: %s", text)
			result := terminal.Result
			require.Equal(t, control.Unknown, result.State, "result: %+v", result)
			require.Equal(t, "typed_incomplete", result.Code, "result: %+v", result)
			require.Equal(t, "complete", result.Cleanup, "result: %+v", result)
			for id, outcome := range tc.outcomes {
				task := taskResult(t, result.Tasks, id)
				require.Equal(t, control.Succeeded, task.State, "%s task: %+v", id, task)
				require.NotNil(t, task.Typed, "%s task: %+v", id, task)
				require.Equal(t, control.Unknown, task.Typed.State, "%s task: %+v", id, task)
				require.Equal(t, "typed_incomplete", task.Typed.Code, "%s task: %+v", id, task)
				require.Equal(t, outcome, task.Typed.Targets[0].Outcome, "%s outcome", id)
			}
		})
	}
}

func TestRawTasksStayOpenWithoutAnAdapterSelection(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := typedPlan(t,
		rawTask("raw-grow", "OperProgress", `{"legacy":true,"future":900719925474099312345}`),
		rawTask("raw-theme", "SwitchTheme", `{"themes":["Not Interpreted"]}`),
		rawTask("raw-copilot", "Copilot", `{"copilot_list":[{"filename":"job.json"}]}`),
		rawTask("raw-roguelike", "Roguelike", `{"theme":"Phantom","mode":0,"starts_count":1}`),
	)
	input := encodePlan(t, plan)

	code, text, lines, diagnostics := runMachine(t, cliArgs("dry-run", executable, dir), input)
	require.Equal(t, exitSuccess, code, "raw preview: %s (%s)", text, diagnostics)
	require.Len(t, lines, 1, "raw preview: %s (%s)", text, diagnostics)
	var preview envelope
	require.NoError(t, json.Unmarshal(lines[0], &preview), "bad preview: %s", text)
	require.NotNil(t, preview.Plan, "bad preview: %s", text)
	for i := range preview.Plan.Tasks {
		require.Nil(t, preview.Tasks[i].Typed, "raw task %d gained a typed preview: %+v", i, preview.Tasks[i].Typed)
	}
	joined := strings.Join(preview.Unverified, "\n")
	require.NotContains(t, joined, "oper_progress:", "raw task names were interpreted as adapter selections: %v", preview.Unverified)
	require.NotContains(t, joined, "switch_theme:", "raw task names were interpreted as adapter selections: %v", preview.Unverified)
	require.NotContains(t, joined, "copilot:", "raw task names were interpreted as adapter selections: %v", preview.Unverified)
	require.NotContains(t, joined, "roguelike:", "raw task names were interpreted as adapter selections: %v", preview.Unverified)

	code, text, lines, diagnostics = runMachine(t, cliArgs("run", executable, dir), input)
	require.Equal(t, exitSuccess, code, "raw run: diagnostics %s (%s)", diagnostics, text)
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal: %s", text)
	require.NotNil(t, terminal.Result, "missing terminal: %s", text)
	require.Equal(t, control.Succeeded, terminal.Result.State, "raw result: %+v", terminal.Result)
	for _, id := range []string{"raw-grow", "raw-theme", "raw-copilot", "raw-roguelike"} {
		task := taskResult(t, terminal.Result.Tasks, id)
		require.Nil(t, task.Typed, "%s gained typed facts: %+v", id, task.Typed)
	}
	for _, line := range lines {
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &event); err == nil && event.Type == "typed" {
			require.FailNow(t, "raw task emitted a typed event: %s", line)
		}
	}
	record, err := control.ReadRecord(dir, terminal.Result.RunID)
	require.NoError(t, err)
	require.Contains(t, string(record.Plan.Tasks[0].Params), "900719925474099312345", "raw record params changed: %s", record.Plan.Tasks[0].Params)
}

func TestTypedAdapterRejectionsLeaveNoRuntimeOrDirectory(t *testing.T) {
	cases := []struct {
		name string
		task control.Task
	}{
		{"duplicate operator", operProgressTask("grow", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Warrior","name":"Alpha","skill_level":7}]}`)},
		{"multiple actions", operProgressTask("grow", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2,"skill_level":7}]}`)},
		{"null action", operProgressTask("grow", `{"plans":[{"role":"Warrior","name":"Alpha","elite":null}]}`)},
		{"unknown field", operProgressTask("grow", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2,"enable":true}]}`)},
		{"wrong core type", strictTask("grow", "SwitchTheme", "oper_progress", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)},
		{"unknown adapter", strictTask("grow", "OperProgress", "future_adapter", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)},
		{"unknown version", control.Task{ID: "grow", Type: "OperProgress", Adapter: adapterSelection("oper_progress", 2), Params: json.RawMessage(`{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)}},
		{"duplicate theme", switchThemeTask("theme", `{"themes":["Theme One","Theme One"]}`)},
		{"blank theme", switchThemeTask("theme", `{"themes":[" Theme One"]}`)},
		{"non-string theme", switchThemeTask("theme", `{"themes":[1]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, _, dir := typedPlan(t, tc.task)
			input := encodePlan(t, plan)
			missingWorker := filepath.Join(t.TempDir(), "missing-worker")
			file := planFile(t, input)
			for _, source := range []struct{ name, input string }{
				{"stdin", "-"},
				{"file", file},
			} {
				commands := []struct {
					name string
					args []string
				}{
					{"dry-run", withInput(cliArgs("dry-run", missingWorker, dir), source.input)},
					{"run", withInput(cliArgs("run", missingWorker, dir), source.input)},
					{"run --dry-run", append(withInput(cliArgs("run", missingWorker, dir), source.input), "--dry-run")},
				}
				for _, command := range commands {
					t.Run(source.name+"/"+command.name, func(t *testing.T) {
						code, text, lines, diagnostics := runMachine(t, command.args, input)
						require.Equal(t, exitRejected, code, "diagnostics: %s (%s)", diagnostics, text)
						assertRejection(t, lines, text)
						_, err := os.Stat(dir)
						require.ErrorIs(t, err, os.ErrNotExist, "rejected command created the data directory")
					})
				}
			}
		})
	}
}

func assertRejection(t *testing.T, lines []json.RawMessage, text string) {
	t.Helper()
	require.Len(t, lines, 1, "rejection wrote %d records: %s", len(lines), text)
	var failure envelope
	require.NoError(t, json.Unmarshal(lines[0], &failure))
	require.Equal(t, "error", failure.Type, "rejection record: %+v", failure)
	require.Equal(t, "invalid_plan", failure.Code, "rejection record: %+v", failure)
	require.Nil(t, failure.Result, "rejection record: %+v", failure)
	require.Nil(t, failure.Plan, "rejection record: %+v", failure)
}

func TestDoctorWithTypedPlanOnlyInitializesAndCloses(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "doctor")
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
		switchThemeTask("theme-1", `{"themes":["Theme One","Theme Two"]}`),
	)
	code, text, _, diagnostics := runMachine(t, cliArgs("doctor", executable, dir), encodePlan(t, plan))
	require.Equal(t, exitSuccess, code, "diagnostics: %s (%s)", diagnostics, text)
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Equal(t, "initialize\nclose\n", string(commands), "doctor commands: %q", commands)
}

func TestTypedEventsCarryOnlyFilteredBusinessFacts(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
		switchThemeTask("theme-1", `{"themes":["Theme One"]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	require.Equal(t, exitSuccess, code, "diagnostics: %s (%s)", diagnostics, text)
	for _, forbidden := range []string{"private-secret", `"rect"`, `"score"`, `"secret"`} {
		require.NotContains(t, text, forbidden, "default output disclosed %q: %s", forbidden, text)
	}
	allowed := map[string]map[string]bool{
		"op-0001":         {"action": true, "expected": true, "slot": true, "reported": true},
		"theme-selection": {"theme": true},
	}
	typed := 0
	for _, line := range lines {
		var event struct {
			Type  string               `json:"type"`
			Typed *control.TypedResult `json:"typed"`
		}
		require.NoError(t, json.Unmarshal(line, &event))
		if event.Type != "typed" {
			continue
		}
		typed++
		require.NotNil(t, event.Typed, "typed event without a typed result: %s", line)
		for _, target := range event.Typed.Targets {
			if len(target.Details) == 0 {
				continue
			}
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(target.Details, &fields), "target details must be a JSON object: %s", target.Details)
			keys, ok := allowed[target.ID]
			require.True(t, ok, "unexpected typed target: %+v", target)
			for key := range fields {
				require.True(t, keys[key], "target %s disclosed field %q: %s", target.ID, key, target.Details)
			}
		}
	}
	require.NotEqual(t, 0, typed, "no typed events: %s", text)
}
