package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
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
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
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
	code := runCLI(context.Background(), args, bytes.NewReader(input), &out, &diagnostics)
	return code, out.String(), decodeLines(t, out.Bytes()), diagnostics.String()
}

func taskResult(t *testing.T, tasks []control.TaskResult, id string) control.TaskResult {
	t.Helper()
	for _, task := range tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %q missing from result", id)
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
	if code := runCLI(context.Background(), []string{"capabilities"}, strings.NewReader(""), &out, &diagnostics); code != exitSuccess {
		t.Fatalf("exit = %d: %s", code, diagnostics.String())
	}
	var caps capabilities
	if err := json.Unmarshal(out.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	adapters := cliAdapters()
	if len(caps.TaskAdapters) != len(adapters) {
		t.Fatalf("adapter capabilities = %d, registered adapters = %d", len(caps.TaskAdapters), len(adapters))
	}
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
		if got.Name != ref.Name || got.Version != ref.Version {
			t.Fatalf("capability %d = %+v, descriptor = %+v", i, got, ref)
		}
		if got.CoreType != wantCore[got.Name] {
			t.Fatalf("capability %d lacks core type: %+v", i, got)
		}
		if !reflect.DeepEqual(got.ObjectivePolicies, wantPolicies[got.Name]) {
			t.Fatalf("capability %d objective policies = %v, want %v", i, got.ObjectivePolicies, wantPolicies[got.Name])
		}
		if got.ResultInterpretation != wantInterpretation[got.Name] {
			t.Fatalf("capability %d interpretation = %q, want %q", i, got.ResultInterpretation, wantInterpretation[got.Name])
		}
		for _, fragment := range wantInput[got.Name] {
			if !strings.Contains(got.Input, fragment) {
				t.Fatalf("capability %d input lacks %q: %q", i, fragment, got.Input)
			}
		}
		for _, fragment := range wantSelection[got.Name] {
			if !strings.Contains(got.PolicySelection, fragment) {
				t.Fatalf("capability %d policy selection lacks %q: %q", i, fragment, got.PolicySelection)
			}
		}
		if !strings.Contains(got.Acceptance, "device acceptance: none") {
			t.Fatalf("capability %d claims acceptance: %q", i, got.Acceptance)
		}
	}
	// Input-only branches are named as none inside the descriptive fields; the
	// capability report keeps no single static objective_policy field.
	if bytes.Contains(out.Bytes(), []byte(`"objective_policy"`)) {
		t.Fatalf("capabilities kept a single static objective_policy field: %s", out.String())
	}
	if got := caps.TaskAdapters[2]; !strings.Contains(got.Acceptance, "does not prove a clear or a safe device handoff") {
		t.Fatalf("copilot acceptance lacks the single-file caveat: %q", got.Acceptance)
	}
	if got := caps.TaskAdapters[3]; !strings.Contains(got.Acceptance, "modes without a reliable terminal fact") {
		t.Fatalf("roguelike acceptance lacks the input-only modes: %q", got.Acceptance)
	}
	if !strings.Contains(caps.AdapterBasis, "9b75f9f7") || !strings.Contains(caps.AdapterBasis, "not executed") {
		t.Fatalf("adapter basis = %q", caps.AdapterBasis)
	}
	if strings.Contains(caps.AdapterBasis, "v6.20.1") {
		t.Fatalf("source review conflates the release candidate: %q", caps.AdapterBasis)
	}
	for _, fragment := range []string{"rolling", "caller-provided", "no fixed Core version", "old-version compatibility", "doctor/run/record"} {
		if !strings.Contains(caps.CoreVersionPolicy, fragment) {
			t.Fatalf("Core version policy lacks %q: %q", fragment, caps.CoreVersionPolicy)
		}
	}
	if bytes.Contains(out.Bytes(), []byte(`"static_snapshot_basis"`)) || bytes.Contains(out.Bytes(), []byte("v6.20.1")) {
		t.Fatalf("capabilities retains a fixed release snapshot: %s", out.String())
	}
	if !strings.Contains(caps.InstallationFacts, "not inspected") {
		t.Fatalf("installation facts = %q", caps.InstallationFacts)
	}
	if caps.DeviceAcceptance != "none" {
		t.Fatalf("device acceptance = %q", caps.DeviceAcceptance)
	}
	if caps.NativeTaskDiscovery || caps.TaskInput == "" || caps.TaskAcceptance == "" {
		t.Fatalf("generic capabilities changed: %+v", caps)
	}

	out.Reset()
	diagnostics.Reset()
	if code := runCLI(context.Background(), []string{"help"}, strings.NewReader(""), &out, &diagnostics); code != exitSuccess {
		t.Fatalf("help exit = %d", code)
	}
	var help envelope
	if err := json.Unmarshal(out.Bytes(), &help); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oper_progress@1", "switch_theme@1", "copilot@1", "roguelike@1", "task.adapter"} {
		if !strings.Contains(help.Usage, name) {
			t.Fatalf("help does not mention %q: %s", name, help.Usage)
		}
	}
	if !strings.Contains(help.Usage, "actual objective policy") || strings.Contains(help.Usage, "Both adapters") {
		t.Fatalf("help does not state the per-task policy preview: %s", help.Usage)
	}
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
	if len(specs) != len(want) {
		t.Fatalf("registered %d adapter specs, want %d", len(specs), len(want))
	}
	for i, spec := range specs {
		if got := spec.newAdapter().Descriptor(); got != want[i] {
			t.Fatalf("spec %d descriptor = %+v, want %+v", i, got, want[i])
		}
	}
	adapters := cliAdapters()
	if len(adapters) != len(want) {
		t.Fatalf("registry has %d adapters, want %d", len(adapters), len(want))
	}
	for i, adapter := range adapters {
		if got := adapter.Descriptor(); got != want[i] {
			t.Fatalf("registry adapter %d = %+v, want %+v", i, got, want[i])
		}
	}
	if got, wantSummary := cliAdapterSummary(),
		"oper_progress@1 (OperProgress), switch_theme@1 (SwitchTheme), copilot@1 (Copilot), roguelike@1 (Roguelike)"; got != wantSummary {
		t.Fatalf("help summary = %q, want %q", got, wantSummary)
	}
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
	if code != exitSuccess {
		t.Fatalf("exit = %d: %s (%s)", code, text, diagnostics)
	}
	if len(lines) != 1 {
		t.Fatalf("dry-run wrote %d records", len(lines))
	}
	var preview envelope
	if err := json.Unmarshal(lines[0], &preview); err != nil || preview.Type != "plan" || preview.Plan == nil || len(preview.Tasks) != 2 {
		t.Fatalf("missing compiled preview: %s", text)
	}
	list := preview.Tasks[0]
	if list.Policy != control.ObjectiveAll || list.Typed == nil || list.Typed.Policy != list.Policy {
		t.Fatalf("copilot list policy = %+v", list)
	}
	if got := targetIDs(list.Typed); !reflect.DeepEqual(got, []string{"copilot:0", "copilot:1"}) {
		t.Fatalf("copilot list targets = %v", got)
	}
	for _, target := range list.Typed.Targets {
		if target.Outcome != control.TargetUnobserved || target.Code != "" || len(target.Details) != 0 {
			t.Fatalf("copilot list preview claims a fact: %+v", target)
		}
	}
	single := preview.Tasks[1]
	if single.Policy != control.ObjectiveNone || single.Typed != nil {
		t.Fatalf("single-file copilot task = %+v", single)
	}
	if got := string(preview.Plan.Tasks[0].Params); !strings.Contains(got, `"id":0`) ||
		!strings.Contains(got, `"id":1`) || !strings.Contains(got, `"is_raid":true`) {
		t.Fatalf("compiled list lost explicit ids or raid flag: %s", got)
	}
	if got := string(preview.Plan.Tasks[1].Params); !strings.Contains(got, `"loop_times":3`) || strings.Contains(got, "copilot_list") {
		t.Fatalf("compiled single-file input changed: %s", got)
	}
	if !strings.Contains(text, `"objective_policy":"all"`) || !strings.Contains(text, `"objective_policy":"none"`) {
		t.Fatalf("dry-run did not report both per-task policies: %s", text)
	}
	joined := strings.Join(preview.Unverified, "\n")
	if !strings.Contains(joined, "copilot:") {
		t.Fatalf("copilot caveats missing: %v", preview.Unverified)
	}
	for _, fragment := range []string{"never inspected during preparation", "no authorized device acceptance", "does not prove a clear or a safe device handoff"} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("copilot caveats lack %q: %v", fragment, preview.Unverified)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dry-run created the data directory: %v", err)
	}
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
	if code != exitSuccess {
		t.Fatalf("exit = %d: %s (%s)", code, text, diagnostics)
	}
	var preview envelope
	if err := json.Unmarshal(lines[0], &preview); err != nil || preview.Type != "plan" || len(preview.Tasks) != 7 {
		t.Fatalf("missing compiled preview: %s", text)
	}
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
		if task.Policy != want.policy {
			t.Fatalf("task %d policy = %q, want %q", i, task.Policy, want.policy)
		}
		if want.policy == control.ObjectiveNone {
			if task.Typed != nil {
				t.Fatalf("input-only task %d gained a typed preview: %+v", i, task.Typed)
			}
			continue
		}
		if task.Typed == nil || task.Typed.Policy != task.Policy {
			t.Fatalf("task %d lacks its typed preview: %+v", i, task)
		}
		if got := targetIDs(task.Typed); !reflect.DeepEqual(got, []string{want.target}) {
			t.Fatalf("task %d targets = %v, want [%s]", i, got, want.target)
		}
	}
	if got := targetIDs(preview.Tasks[6].Typed); !reflect.DeepEqual(got, []string{"roguelike:run:1", "roguelike:run:2", "roguelike:run:3"}) {
		t.Fatalf("multi-run targets = %v", got)
	}
	if preview.Tasks[6].Typed.Policy != control.ObjectiveAny {
		t.Fatalf("multi-run policy = %q, want any", preview.Tasks[6].Typed.Policy)
	}
	joined := strings.Join(preview.Unverified, "\n")
	if !strings.Contains(joined, "roguelike:") || !strings.Contains(joined, "no authorized device acceptance") {
		t.Fatalf("roguelike caveats missing: %v", preview.Unverified)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dry-run created the data directory: %v", err)
	}
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
			if code != exitSuccess {
				t.Fatalf("exit = %d: %s", code, diagnostics)
			}
			if len(lines) != 1 {
				t.Fatalf("dry-run wrote %d records", len(lines))
			}
			var preview envelope
			if err := json.Unmarshal(lines[0], &preview); err != nil {
				t.Fatal(err)
			}
			if preview.Type != "plan" || preview.Plan == nil || len(preview.Tasks) != 3 {
				t.Fatalf("missing compiled preview: %s", text)
			}
			if got := string(preview.Plan.Tasks[0].Params); got != `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}` {
				t.Fatalf("compiled plans = %s", got)
			}
			if got := string(preview.Plan.Tasks[1].Params); got != `{"themes":["Theme One","Theme Two"]}` {
				t.Fatalf("compiled themes = %s", got)
			}
			if got := string(preview.Plan.Tasks[2].Params); got != `{"future":900719925474099312345}` {
				t.Fatalf("raw params changed = %s", got)
			}
			if got := targetIDs(preview.Tasks[0].Typed); !reflect.DeepEqual(got, []string{"op-0001", "op-0002"}) {
				t.Fatalf("operator targets = %v", got)
			}
			if got := targetIDs(preview.Tasks[1].Typed); !reflect.DeepEqual(got, []string{"theme-selection"}) {
				t.Fatalf("theme targets = %v", got)
			}
			for _, task := range preview.Tasks[:2] {
				if task.Policy != control.ObjectiveAll || task.Typed == nil || task.Typed.Policy != task.Policy || task.Typed.State != control.Unknown || task.Typed.Code != "typed_incomplete" {
					t.Fatalf("prepared task lacks an unobserved aggregate: %+v", task)
				}
				for _, target := range task.Typed.Targets {
					if target.Outcome != control.TargetUnobserved || target.Code != "" || len(target.Details) != 0 {
						t.Fatalf("preview claims a fact: %+v", target)
					}
				}
			}
			if preview.Tasks[2].Typed != nil {
				t.Fatalf("raw task gained a typed result: %+v", preview.Tasks[2].Typed)
			}
			joined := strings.Join(preview.Unverified, "\n")
			if !strings.Contains(joined, "oper_progress:") || !strings.Contains(joined, "switch_theme:") {
				t.Fatalf("adapter caveats missing: %v", preview.Unverified)
			}
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
				if !strings.Contains(joined, fragment) {
					t.Fatalf("preview unverified conditions lack %q: %v", fragment, preview.Unverified)
				}
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("dry-run created the data directory: %v", err)
			}
			if _, err := os.Stat(trace); !os.IsNotExist(err) {
				t.Fatalf("dry-run launched a worker: %v", err)
			}
		})
	}

	// run --dry-run is the same side-effect-free preview.
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	args := append(cliArgs("run", missingWorker, dir), "--dry-run")
	code, text, lines, diagnostics := runMachine(t, args, input)
	if code != exitSuccess || len(lines) != 1 {
		t.Fatalf("run --dry-run exit = %d: %s (%s)", code, text, diagnostics)
	}
	var preview envelope
	if err := json.Unmarshal(lines[0], &preview); err != nil || preview.Type != "plan" || preview.Plan == nil {
		t.Fatalf("run --dry-run did not preview: %s", text)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("run --dry-run created the data directory: %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("run --dry-run launched a worker: %v", err)
	}
}

func readTaskTrace(t *testing.T, path string) [][2]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	out := make([][2]string, len(lines))
	for i, line := range lines {
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 {
			t.Fatalf("malformed task trace line: %q", line)
		}
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
			if code != exitSuccess || len(lines) != 1 {
				t.Fatalf("preview exit = %d: %s (%s)", code, text, diagnostics)
			}
			var preview envelope
			if err := json.Unmarshal(lines[0], &preview); err != nil || preview.Plan == nil || len(preview.Tasks) != 3 {
				t.Fatalf("bad preview: %s", text)
			}

			code, text, lines, diagnostics = runMachine(t, withInput(cliArgs("run", executable, dir), source), input)
			if code != exitSuccess {
				t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
			}
			var terminal envelope
			if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Type != "terminal" || terminal.Result == nil {
				t.Fatalf("missing terminal record: %s", text)
			}
			result := terminal.Result
			if result.State != control.Succeeded || result.Code != "" || result.Cleanup != "complete" {
				t.Fatalf("result = %+v", result)
			}
			grow := taskResult(t, result.Tasks, "grow-1")
			if grow.State != control.Succeeded || grow.Typed == nil || grow.Typed.State != control.Succeeded {
				t.Fatalf("operator task = %+v", grow)
			}
			if got := targetIDs(grow.Typed); !reflect.DeepEqual(got, targetIDs(preview.Tasks[0].Typed)) {
				t.Fatalf("preview targets %v, terminal targets %v", targetIDs(preview.Tasks[0].Typed), got)
			}
			if target := grow.Typed.Targets[0]; target.ID != "op-0001" || target.Outcome != control.TargetAchieved || target.Code != "completed" {
				t.Fatalf("operator fact = %+v", target)
			}
			theme := taskResult(t, result.Tasks, "theme-1")
			if theme.State != control.Succeeded || theme.Typed == nil || theme.Typed.State != control.Succeeded {
				t.Fatalf("theme task = %+v", theme)
			}
			if got := targetIDs(theme.Typed); !reflect.DeepEqual(got, targetIDs(preview.Tasks[1].Typed)) {
				t.Fatalf("preview targets %v, terminal targets %v", targetIDs(preview.Tasks[1].Typed), got)
			}
			if target := theme.Typed.Targets[0]; target.ID != "theme-selection" || target.Outcome != control.TargetAlreadySet || target.Code != "already_set" {
				t.Fatalf("theme fact = %+v", target)
			}
			if raw := taskResult(t, result.Tasks, "raw-1"); raw.State != control.Succeeded || raw.Typed != nil {
				t.Fatalf("raw task gained typed facts: %+v", raw)
			}

			// The worker received exactly the types and compiled parameters that
			// dry-run previewed; raw parameters were not rewritten.
			submitted := readTaskTrace(t, taskTrace)
			if len(submitted) != len(preview.Plan.Tasks) {
				t.Fatalf("worker received %d tasks, preview has %d", len(submitted), len(preview.Plan.Tasks))
			}
			for i, got := range submitted {
				want := preview.Plan.Tasks[i]
				if got[0] != want.Type || got[1] != string(want.Params) {
					t.Fatalf("task %d submitted %q %s, preview %q %s", i, got[0], got[1], want.Type, want.Params)
				}
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
				if err := json.Unmarshal(line, &event); err != nil {
					t.Fatal(err)
				}
				if event.Type != "typed" {
					continue
				}
				typedEvents++
				if event.Typed == nil || event.TaskID == "" {
					t.Fatalf("typed event without a typed result or task: %s", line)
				}
				if len(event.Detail) != 0 {
					t.Fatalf("typed event disclosed raw callback details: %s", line)
				}
				latest[event.TaskID] = event.Typed
			}
			if typedEvents != 3 {
				t.Fatalf("typed events = %d, want 3 (operator detail, operator summary, theme fact)", typedEvents)
			}
			// The final typed event, terminal result and durable record carry the
			// same task result; no second business state is maintained.
			for id, want := range latest {
				if got := taskResult(t, result.Tasks, id).Typed; !reflect.DeepEqual(got, want) {
					t.Fatalf("%s event and terminal typed result disagree: %+v vs %+v", id, want, got)
				}
			}

			record, err := control.ReadRecord(dir, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(record.Result.Tasks, result.Tasks) || record.Result.State != result.State || record.Result.Cleanup != result.Cleanup {
				t.Fatalf("terminal and record disagree: %+v vs %+v", record.Result, result)
			}
			if got := string(record.Plan.Tasks[0].Params); !strings.Contains(got, `"name":"Alpha","role":"Warrior"`) {
				t.Fatalf("record did not keep the caller plan: %s", got)
			}
			if got := string(record.Plan.Tasks[2].Params); !strings.Contains(got, "900719925474099312345") {
				t.Fatalf("record lost the raw integer literal: %s", got)
			}
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
	if code != exitFailed {
		t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
	}
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal: %s", text)
	}
	result := terminal.Result
	if result.State != control.Failed || result.Code != "typed_failed" || result.Cleanup != "complete" {
		t.Fatalf("result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.State != control.Succeeded {
		t.Fatalf("typed failure rewrote the chain fact: %+v", grow)
	}
	if target := grow.Typed.Targets[0]; target.Outcome != control.TargetFailed || target.Code != "resource_insufficient" {
		t.Fatalf("operator failure = %+v", target)
	}
	theme := taskResult(t, result.Tasks, "theme-1")
	if theme.State != control.Succeeded || theme.Typed.State != control.Failed {
		t.Fatalf("theme result = %+v", theme)
	}
	if target := theme.Typed.Targets[0]; target.Outcome != control.TargetLocked || target.Code != "locked" {
		t.Fatalf("theme fact = %+v", target)
	}
}

func TestTypedFactsSurviveWorkerFaultAsUnknown(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "fault")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","skill_level":7}]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
	}
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal: %s", text)
	}
	result := terminal.Result
	if result.State != control.Unknown || result.Code != "instance_fault" || result.Cleanup != "complete" {
		t.Fatalf("fault result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.State == control.Succeeded {
		t.Fatalf("fault left a successful chain fact: %+v", grow)
	}
	if grow.Typed == nil || grow.Typed.State != control.Failed {
		t.Fatalf("confirmed typed fact was lost on fault: %+v", grow)
	}
	if target := grow.Typed.Targets[0]; target.Outcome != control.TargetFailed || target.Code != "resource_insufficient" {
		t.Fatalf("typed target fact = %+v", target)
	}
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
	code := runCLI(ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s", code, diagnostics.String())
	}
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal: %s", out.String())
	}
	result := terminal.Result
	// A worker that reports a Close error and exits immediately can be observed
	// either through that error or through the following disconnect. Both are
	// cleanup failures, and neither may rewrite the confirmed stop.
	if result.State != control.Stopped || result.Cleanup != "failed" ||
		(result.CleanupCode != "close_failed" && result.CleanupCode != "disconnected") {
		t.Fatalf("confirmed stop was rewritten by cleanup: %+v", result)
	}
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
	code := runCLI(ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s", code, diagnostics.String())
	}
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal: %s", out.String())
	}
	result := terminal.Result
	// As above, the close error and the disconnect are transport-equivalent
	// cleanup failures; the established typed failure and its target fact must
	// survive either observation.
	if result.State != control.Failed || result.Code != "typed_failed" ||
		result.Cleanup != "failed" ||
		(result.CleanupCode != "close_failed" && result.CleanupCode != "disconnected") {
		t.Fatalf("established failure was rewritten by cleanup: %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.Typed == nil || grow.Typed.State != control.Failed || grow.Typed.Targets[0].Outcome != control.TargetFailed {
		t.Fatalf("typed failure was lost: %+v", grow)
	}
}

func TestEstablishedFailureSurvivesFaultAndCleanupFailure(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "prooffault")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","skill_level":7}]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
	}
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal: %s", text)
	}
	result := terminal.Result
	if result.State != control.Failed || result.Code != "typed_failed" ||
		result.Cleanup != "failed" || result.CleanupCode != "instance_fault" {
		t.Fatalf("proof state was rewritten by fault cleanup: %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.State != control.Succeeded || grow.Typed == nil || grow.Typed.State != control.Failed {
		t.Fatalf("chain and typed facts changed: %+v", grow)
	}
}

func TestRecordWriteFailuresUsePersistenceExitCode(t *testing.T) {
	for _, result := range []control.Result{
		{State: control.Rejected, Code: "record_write_failed", Cleanup: "complete"},
		{State: control.Succeeded, Cleanup: "complete", CleanupCode: "record_write_failed"},
	} {
		if code := resultExit(result); code != exitUnknown {
			t.Fatalf("resultExit(%+v) = %d, want %d", result, code, exitUnknown)
		}
	}
	if code := resultExit(control.Result{State: control.Rejected, Code: "invalid_plan", Cleanup: "complete"}); code != exitRejected {
		t.Fatalf("ordinary rejection exit = %d", code)
	}
	wrapped := fmt.Errorf("admission: %w", control.ErrRecordWrite)
	if code := errorExit(wrapped); code != exitUnknown {
		t.Fatalf("errorExit(ErrRecordWrite) = %d", code)
	}
	if code := errorCode(wrapped); code != "record_write_failed" {
		t.Fatalf("errorCode(ErrRecordWrite) = %q", code)
	}
}

func TestAdmissionPersistenceFailureIsClassified(t *testing.T) {
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	rt, err := control.New(control.Config{DataDir: dir, Adapters: cliAdapters(), Worker: worker.Config{Executable: executable}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	runs := filepath.Join(dir, "runs")
	if err := os.RemoveAll(runs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runs, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), plan); !errors.Is(err, control.ErrRecordWrite) {
		t.Fatalf("admission error = %v", err)
	} else if code := errorCode(err); code != "record_write_failed" {
		t.Fatalf("admission error code = %q", code)
	} else if exit := errorExit(err); exit != exitUnknown {
		t.Fatalf("admission exit = %d", exit)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	stopRead := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stopRead()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
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
		if err := os.RemoveAll(runs); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runs, []byte("blocked"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Fatal(err)
		}
		sabotaged = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("CLI did not finish within its deadline: %v", ctx.Err())
	}
	if !sabotaged {
		t.Fatalf("CLI never reached admission: %s", diagnostics.String())
	}
	if cmd.ProcessState == nil {
		t.Fatalf("CLI did not exit: %v diagnostics=%s", waitErr, diagnostics.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != exitUnknown {
		t.Fatalf("exit = %d (%v): %s", code, waitErr, diagnostics.String())
	}
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal record: %v", err)
	}
	// The post-initialize save fails first. While the run directory is still
	// blocked the terminal save fails too, so the final result may be
	// rejected or unknown; both must carry record_write_failed and exit 6.
	result := terminal.Result
	if result.Code != "record_write_failed" ||
		(result.State != control.Rejected && result.State != control.Unknown) {
		t.Fatalf("persistence failure result = %+v", result)
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"connect", "append_task", "start"} {
		if bytes.Contains(commands, []byte(forbidden+"\n")) {
			t.Fatalf("persistence failure issued %q: %s", forbidden, commands)
		}
	}
	if !bytes.Contains(commands, []byte("initialize\n")) {
		t.Fatalf("worker was not initialized: %s", commands)
	}
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
			if code != exitUnknown {
				t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
			}
			var terminal envelope
			if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
				t.Fatalf("missing terminal: %s", text)
			}
			result := terminal.Result
			if result.State != control.Unknown || result.Code != "typed_incomplete" || result.Cleanup != "complete" {
				t.Fatalf("result = %+v", result)
			}
			for id, outcome := range tc.outcomes {
				task := taskResult(t, result.Tasks, id)
				if task.State != control.Succeeded || task.Typed == nil || task.Typed.State != control.Unknown || task.Typed.Code != "typed_incomplete" {
					t.Fatalf("%s task = %+v", id, task)
				}
				if got := task.Typed.Targets[0].Outcome; got != outcome {
					t.Fatalf("%s outcome = %q, want %q", id, got, outcome)
				}
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
	if code != exitSuccess || len(lines) != 1 {
		t.Fatalf("raw preview exit = %d: %s (%s)", code, text, diagnostics)
	}
	var preview envelope
	if err := json.Unmarshal(lines[0], &preview); err != nil || preview.Plan == nil {
		t.Fatalf("bad preview: %s", text)
	}
	for i := range preview.Plan.Tasks {
		if preview.Tasks[i].Typed != nil {
			t.Fatalf("raw task %d gained a typed preview: %+v", i, preview.Tasks[i].Typed)
		}
	}
	if joined := strings.Join(preview.Unverified, "\n"); strings.Contains(joined, "oper_progress:") ||
		strings.Contains(joined, "switch_theme:") || strings.Contains(joined, "copilot:") || strings.Contains(joined, "roguelike:") {
		t.Fatalf("raw task names were interpreted as adapter selections: %v", preview.Unverified)
	}

	code, text, lines, diagnostics = runMachine(t, cliArgs("run", executable, dir), input)
	if code != exitSuccess {
		t.Fatalf("raw run exit = %d: %s (%s)", code, diagnostics, text)
	}
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal: %s", text)
	}
	if terminal.Result.State != control.Succeeded {
		t.Fatalf("raw result = %+v", terminal.Result)
	}
	for _, id := range []string{"raw-grow", "raw-theme", "raw-copilot", "raw-roguelike"} {
		if task := taskResult(t, terminal.Result.Tasks, id); task.Typed != nil {
			t.Fatalf("%s gained typed facts: %+v", id, task.Typed)
		}
	}
	for _, line := range lines {
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &event); err == nil && event.Type == "typed" {
			t.Fatalf("raw task emitted a typed event: %s", line)
		}
	}
	record, err := control.ReadRecord(dir, terminal.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(record.Plan.Tasks[0].Params); !strings.Contains(got, "900719925474099312345") {
		t.Fatalf("raw record params changed: %s", got)
	}
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
						if code != exitRejected {
							t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
						}
						assertRejection(t, lines, text)
						if _, err := os.Stat(dir); !os.IsNotExist(err) {
							t.Fatalf("rejected command created the data directory: %v", err)
						}
					})
				}
			}
		})
	}
}

func assertRejection(t *testing.T, lines []json.RawMessage, text string) {
	t.Helper()
	if len(lines) != 1 {
		t.Fatalf("rejection wrote %d records: %s", len(lines), text)
	}
	var failure envelope
	if err := json.Unmarshal(lines[0], &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Type != "error" || failure.Code != "invalid_plan" || failure.Result != nil || failure.Plan != nil {
		t.Fatalf("rejection record = %+v", failure)
	}
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
	if code != exitSuccess {
		t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "initialize\nclose\n" {
		t.Fatalf("doctor commands = %q", commands)
	}
}

func TestTypedEventsCarryOnlyFilteredBusinessFacts(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
		switchThemeTask("theme-1", `{"themes":["Theme One"]}`),
	)
	code, text, lines, diagnostics := runMachine(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitSuccess {
		t.Fatalf("exit = %d: %s (%s)", code, diagnostics, text)
	}
	for _, forbidden := range []string{"private-secret", `"rect"`, `"score"`, `"secret"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("default output disclosed %q: %s", forbidden, text)
		}
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
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "typed" {
			continue
		}
		typed++
		if event.Typed == nil {
			t.Fatalf("typed event without a typed result: %s", line)
		}
		for _, target := range event.Typed.Targets {
			if len(target.Details) == 0 {
				continue
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(target.Details, &fields); err != nil {
				t.Fatalf("target details must be a JSON object: %s", target.Details)
			}
			keys, ok := allowed[target.ID]
			if !ok {
				t.Fatalf("unexpected typed target: %+v", target)
			}
			for key := range fields {
				if !keys[key] {
					t.Fatalf("target %s disclosed field %q: %s", target.ID, key, target.Details)
				}
			}
		}
	}
	if typed == 0 {
		t.Fatalf("no typed events: %s", text)
	}
}
