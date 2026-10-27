package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSelectLayers(t *testing.T) {
	defs := definitions()
	all := make([]string, 0, len(defs))
	for _, def := range defs {
		all = append(all, def.Name)
	}
	tests := []struct {
		name    string
		spec    string
		want    []string
		wantErr bool
	}{
		{name: "default is ci", spec: "", want: []string{"abi", "build", "callback", "fmt", "go", "loader", "static"}},
		{name: "all", spec: "all", want: sortedStrings(all)},
		{name: "explicit", spec: "static,core", want: []string{"core", "static"}},
		{name: "duplicate deduplicated", spec: "static,static", want: []string{"static"}},
		{name: "unknown", spec: "nope", wantErr: true},
		{name: "empty tokens", spec: " , ", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectLayers(defs, tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("selectLayers(%q) succeeded, want error", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectLayers(%q): %v", tc.spec, err)
			}
			if !reflect.DeepEqual(sortedKeys(got), tc.want) {
				t.Fatalf("selectLayers(%q) = %v, want %v", tc.spec, sortedKeys(got), tc.want)
			}
		})
	}
}

func TestCISelectionDoesNotClaimRealCore(t *testing.T) {
	selected, err := selectLayers(definitions(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"core", "race"} {
		if selected[name] {
			t.Fatalf("ci selection must not include %q", name)
		}
	}
	for _, name := range []string{"static", "go", "loader", "abi", "callback", "build", "fmt"} {
		if !selected[name] {
			t.Fatalf("ci selection must include %q", name)
		}
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		selected []string
		layers   []layerResult
		want     int
	}{
		{name: "all pass", selected: []string{"a"}, layers: []layerResult{{Name: "a", Status: statusPass}}, want: 0},
		{name: "expected skip is pass", selected: []string{"a"}, layers: []layerResult{{Name: "a", Status: statusPassExpected}}, want: 0},
		{name: "not selected ignored", selected: []string{"a"}, layers: []layerResult{{Name: "a", Status: statusPass}, {Name: "b", Status: statusNotSelected}}, want: 0},
		{name: "unverified", selected: []string{"a", "b"}, layers: []layerResult{{Name: "a", Status: statusPass}, {Name: "b", Status: statusUnverified}}, want: 2},
		{name: "fail", selected: []string{"a", "b"}, layers: []layerResult{{Name: "a", Status: statusPass}, {Name: "b", Status: statusFail}}, want: 1},
		{name: "fail dominates unverified", selected: []string{"a", "b"}, layers: []layerResult{{Name: "a", Status: statusUnverified}, {Name: "b", Status: statusFail}}, want: 1},
		{name: "missing selected result", selected: []string{"a", "b"}, layers: []layerResult{{Name: "a", Status: statusPass}}, want: 1},
		{name: "selected but not run", selected: []string{"a"}, layers: []layerResult{{Name: "a", Status: statusNotSelected}}, want: 1},
		{name: "empty selection", selected: nil, layers: nil, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(report{Selected: tc.selected, Layers: tc.layers}); got != tc.want {
				t.Fatalf("exitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestConclusionReportsExpectedSkips(t *testing.T) {
	results := []layerResult{{Name: "core", Status: statusPassExpected, Skipped: 1}}
	got := conclusion(map[string]bool{"core": true}, results)
	if !strings.Contains(got, "expected skips") {
		t.Fatalf("conclusion = %q, want mention of expected skips", got)
	}
}

func TestGoTestArgsIncludesRaceOnlyWhenRequested(t *testing.T) {
	base := goTestStep{Packages: []string{"./x"}}
	if containsArg(goTestArgs(base), "-race") {
		t.Fatal("goTestArgs must not add -race for an ordinary step")
	}
	spec := raceGoTestStep()
	if !spec.Race {
		t.Fatal("raceGoTestStep must set Race: true")
	}
	args := goTestArgs(spec)
	if !containsArg(args, "-race") {
		t.Fatalf("race layer args = %v, want -race", args)
	}
	if spec.Skip != "TestRealCore" {
		t.Fatalf("race layer skip filter = %q, want TestRealCore", spec.Skip)
	}
	if !containsArg(args, "-skip") {
		t.Fatalf("race layer args = %v, want -skip", args)
	}
	for i, arg := range args {
		if arg == "-skip" && (i+1 >= len(args) || args[i+1] != spec.Skip) {
			t.Fatalf("race layer -skip value = %v, want %q", args, spec.Skip)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestRunLayerPreservesDefinitionName(t *testing.T) {
	def := layer{
		Name: "outer",
		Run: func(context.Context, environment) layerResult {
			return newLayerResult("inner-step")
		},
	}
	res := runLayer(context.Background(), environment{}, def)
	if res.Name != "outer" {
		t.Fatalf("runLayer name = %q, want outer", res.Name)
	}
}

func TestLayerResultMergeStatusPrecedence(t *testing.T) {
	var res layerResult
	res.Status = statusPass
	res.merge(layerResult{Status: statusUnverified, Passed: 1, Skipped: 1})
	if res.Status != statusUnverified {
		t.Fatalf("status = %q, want %q", res.Status, statusUnverified)
	}
	res.merge(layerResult{Status: statusFail, Failed: 1})
	if res.Status != statusFail {
		t.Fatalf("status = %q, want %q", res.Status, statusFail)
	}
	if res.Passed != 1 || res.Skipped != 1 || res.Failed != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/1", res.Passed, res.Skipped, res.Failed)
	}
}

func TestRunGoTestClassifiesPassSkipAndFail(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		run        string
		allowSkips []string
		wantStatus string
		wantPassed int
		wantSkip   int
		wantFailed int
	}{
		{
			name:       "pass",
			body:       "func TestPass(t *testing.T) {}",
			wantStatus: statusPass,
			wantPassed: 1,
		},
		{
			name:       "skip is unverified",
			body:       "func TestSkip(t *testing.T) { t.Skip(\"resource missing\") }",
			wantStatus: statusUnverified,
			wantSkip:   1,
		},
		{
			name:       "expected skip",
			body:       "func TestSkip(t *testing.T) { t.Skip(\"resource missing\") }",
			allowSkips: []string{"TestSkip"},
			wantStatus: statusPassExpected,
			wantSkip:   1,
		},
		{
			name:       "fail",
			body:       "func TestFail(t *testing.T) { t.Fatal(\"boom\") }",
			wantStatus: statusFail,
			wantFailed: 1,
		},
		{
			name:       "no tests matched",
			body:       "func TestPass(t *testing.T) {}",
			run:        "TestNoSuchTest",
			wantStatus: statusUnverified,
		},
		{
			name:       "build failure",
			body:       "func TestBad(t *testing.T) { this is not go }",
			wantStatus: statusFail,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTestModule(t, tc.body)
			res := runGoTest(context.Background(), environment{Root: dir}, goTestStep{
				Name:       tc.name,
				Packages:   []string{"."},
				Run:        tc.run,
				AllowSkips: tc.allowSkips,
				Env:        noCGO,
				Timeout:    2 * time.Minute,
			})
			if res.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", res.Status, tc.wantStatus, res.Detail)
			}
			if res.Passed != tc.wantPassed || res.Skipped != tc.wantSkip || res.Failed != tc.wantFailed {
				t.Fatalf("counts = %d/%d/%d, want %d/%d/%d (detail: %s)",
					res.Passed, res.Skipped, res.Failed, tc.wantPassed, tc.wantSkip, tc.wantFailed, res.Detail)
			}
			if tc.wantStatus == statusUnverified && tc.wantSkip > 0 && len(res.UnexpectedSkips) == 0 && len(res.ExpectedSkips) == 0 {
				t.Fatalf("unverified skip must name the skipped test: %s", res.Detail)
			}
		})
	}
}

func writeTestModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/checktest\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	src := "package checktest\n\nimport \"testing\"\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write test: %v", err)
	}
	return dir
}

func TestRunCommandFailOnOutput(t *testing.T) {
	dir := t.TempDir()
	env := environment{Root: dir}
	if res := runCommand(context.Background(), env, "silent", []string{"sh", "-c", "true"}, nil, time.Minute, true); res.Status != statusPass {
		t.Fatalf("silent command status = %q, want %q (%s)", res.Status, statusPass, res.Detail)
	}
	if res := runCommand(context.Background(), env, "noisy", []string{"sh", "-c", "echo file.go"}, nil, time.Minute, true); res.Status != statusFail {
		t.Fatalf("noisy command status = %q, want %q (%s)", res.Status, statusFail, res.Detail)
	}
}

func TestCheckerTargetsUseVendoredInput(t *testing.T) {
	root, err := resolveRoot("")
	if err != nil {
		t.Fatalf("resolveRoot: %v", err)
	}
	env := environment{Root: root}
	bin, cleanup, err := buildChecker(context.Background(), env)
	if err != nil {
		t.Fatalf("buildChecker: %v", err)
	}
	defer cleanup()

	for _, tc := range []struct {
		goos      string
		bound     int
		nonTarget int
	}{
		{goos: "linux", bound: 25, nonTarget: 1},
		{goos: "windows", bound: 26, nonTarget: 0},
	} {
		res := runCheckerTarget(context.Background(), env, bin, tc.goos, tc.bound, tc.nonTarget, 4)
		if res.Status != statusPass {
			t.Fatalf("checker %s status = %q, want %q (%s)", tc.goos, res.Status, statusPass, res.Detail)
		}
	}
}

func TestCheckerTargetRejectsWrongPolicyCounts(t *testing.T) {
	root, err := resolveRoot("")
	if err != nil {
		t.Fatalf("resolveRoot: %v", err)
	}
	env := environment{Root: root}
	bin, cleanup, err := buildChecker(context.Background(), env)
	if err != nil {
		t.Fatalf("buildChecker: %v", err)
	}
	defer cleanup()

	res := runCheckerTarget(context.Background(), env, bin, "linux", 24, 1, 4)
	if res.Status != statusFail {
		t.Fatalf("wrong bound count status = %q, want %q (%s)", res.Status, statusFail, res.Detail)
	}
}

func TestBuildReportOmitsRepositoryRoot(t *testing.T) {
	env := environment{
		Root:      "/home/private-user/rhodeck",
		GoVersion: "go1.26.9",
		GOOS:      "linux",
		GOARCH:    "amd64",
		Purego:    "v0.11.2",
	}
	rep := buildReport(env, map[string]bool{"static": true}, []layerResult{{Name: "static", Status: statusPass}})
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "/home/private-user") {
		t.Fatalf("report leaked the repository root: %s", data)
	}
}

func TestMergeEnvOverridesWithoutDuplicates(t *testing.T) {
	got := mergeEnv([]string{"A=1", "CGO_ENABLED=1", "GOOS=linux"}, "CGO_ENABLED=0", "GOWORK=off")
	want := []string{"A=1", "GOOS=linux", "CGO_ENABLED=0", "GOWORK=off"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnv = %v, want %v", got, want)
	}
}

func TestDefinitionsAreUniqueAndConsistent(t *testing.T) {
	defs := definitions()
	seen := map[string]bool{}
	for _, def := range defs {
		if seen[def.Name] {
			t.Fatalf("duplicate layer %q", def.Name)
		}
		seen[def.Name] = true
		if def.Description == "" {
			t.Fatalf("layer %q has no description", def.Name)
		}
		if def.Run == nil {
			t.Fatalf("layer %q has no run function", def.Name)
		}
		for _, req := range def.Requires {
			switch req {
			case "cc", "tarball":
			default:
				t.Fatalf("layer %q has unknown requirement %q", def.Name, req)
			}
		}
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
