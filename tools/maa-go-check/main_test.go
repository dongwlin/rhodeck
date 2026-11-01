package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
				require.Error(t, err, "selectLayers(%q) succeeded, want error", tc.spec)
				return
			}
			require.NoError(t, err, "selectLayers(%q): %v", tc.spec, err)
			require.Equal(t, tc.want, sortedKeys(got), "selectLayers(%q) = %v, want %v", tc.spec, sortedKeys(got), tc.want)
		})
	}
}

func TestCISelectionDoesNotClaimRealCore(t *testing.T) {
	selected, err := selectLayers(definitions(), "ci")
	require.NoError(t, err)
	for _, name := range []string{"core", "race"} {
		require.False(t, selected[name], "ci selection must not include %q", name)
	}
	for _, name := range []string{"static", "go", "loader", "abi", "callback", "build", "fmt"} {
		require.True(t, selected[name], "ci selection must include %q", name)
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
			got := exitCode(report{Selected: tc.selected, Layers: tc.layers})
			require.Equal(t, tc.want, got, "exitCode() = %d, want %d", got, tc.want)
		})
	}
}

func TestConclusionReportsExpectedSkips(t *testing.T) {
	results := []layerResult{{Name: "core", Status: statusPassExpected, Skipped: 1}}
	got := conclusion(map[string]bool{"core": true}, results)
	require.Contains(t, got, "expected skips", "conclusion = %q, want mention of expected skips", got)
}

func TestGoTestArgsIncludesRaceOnlyWhenRequested(t *testing.T) {
	base := goTestStep{Packages: []string{"./x"}}
	require.False(t, containsArg(goTestArgs(base), "-race"), "goTestArgs must not add -race for an ordinary step")
	spec := raceGoTestStep()
	require.True(t, spec.Race, "raceGoTestStep must set Race: true")
	args := goTestArgs(spec)
	require.True(t, containsArg(args, "-race"), "race layer args = %v, want -race", args)
	require.Equal(t, "TestRealCore", spec.Skip, "race layer skip filter = %q, want TestRealCore", spec.Skip)
	require.True(t, containsArg(args, "-skip"), "race layer args = %v, want -skip", args)
	for i, arg := range args {
		if arg == "-skip" {
			require.Truef(t, i+1 < len(args) && args[i+1] == spec.Skip, "race layer -skip value = %v, want %q", args, spec.Skip)
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
	require.Equal(t, "outer", res.Name, "runLayer name = %q, want outer", res.Name)
}

func TestLayerResultMergeStatusPrecedence(t *testing.T) {
	var res layerResult
	res.Status = statusPass
	res.merge(layerResult{Status: statusUnverified, Passed: 1, Skipped: 1})
	require.Equal(t, statusUnverified, res.Status, "status = %q, want %q", res.Status, statusUnverified)
	res.merge(layerResult{Status: statusFail, Failed: 1})
	require.Equal(t, statusFail, res.Status, "status = %q, want %q", res.Status, statusFail)
	require.Truef(t, res.Passed == 1 && res.Skipped == 1 && res.Failed == 1, "counts = %d/%d/%d, want 1/1/1", res.Passed, res.Skipped, res.Failed)
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
			require.Equal(t, tc.wantStatus, res.Status, "status = %q, want %q (detail: %s)", res.Status, tc.wantStatus, res.Detail)
			require.Truef(t, res.Passed == tc.wantPassed && res.Skipped == tc.wantSkip && res.Failed == tc.wantFailed,
				"counts = %d/%d/%d, want %d/%d/%d (detail: %s)",
				res.Passed, res.Skipped, res.Failed, tc.wantPassed, tc.wantSkip, tc.wantFailed, res.Detail)
			if tc.wantStatus == statusUnverified && tc.wantSkip > 0 {
				require.Truef(t, len(res.UnexpectedSkips) > 0 || len(res.ExpectedSkips) > 0, "unverified skip must name the skipped test: %s", res.Detail)
			}
		})
	}
}

func writeTestModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/checktest\n\ngo 1.25.0\n"), 0o644)
	require.NoError(t, err, "write go.mod")
	src := "package checktest\n\nimport \"testing\"\n\n" + body + "\n"
	err = os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o644)
	require.NoError(t, err, "write test")
	return dir
}

func TestRunCommandFailOnOutput(t *testing.T) {
	dir := t.TempDir()
	env := environment{Root: dir}
	exe, err := os.Executable()
	require.NoError(t, err)
	args := []string{exe, "-test.run=^TestRunCommandHelper$"}
	res := runCommand(context.Background(), env, "silent", args, []string{"MAA_CHECK_COMMAND_HELPER=silent"}, time.Minute, true)
	require.Equal(t, statusPass, res.Status, "silent command status = %q, want %q (%s)", res.Status, statusPass, res.Detail)
	res = runCommand(context.Background(), env, "noisy", args, []string{"MAA_CHECK_COMMAND_HELPER=noisy"}, time.Minute, true)
	require.Equal(t, statusFail, res.Status, "noisy command status = %q, want %q (%s)", res.Status, statusFail, res.Detail)
}

// TestRunCommandHelper supplies portable silent/noisy subprocesses without
// requiring a shell on the host running the tool tests.
func TestRunCommandHelper(t *testing.T) {
	mode := os.Getenv("MAA_CHECK_COMMAND_HELPER")
	if mode == "" {
		return
	}
	if mode == "noisy" {
		_, _ = os.Stdout.WriteString("file.go\n")
	}
	os.Exit(0)
}

func TestCheckerTargetsUseSelectedInput(t *testing.T) {
	root, err := resolveRoot("")
	require.NoError(t, err, "resolveRoot")
	input := t.TempDir()
	source := filepath.Join(root, "tools", "check-maa-api", "testdata", "snapshot")
	for _, name := range []string{"inputs.json", "upstream/AsstCaller.h", "upstream/AsstCallerExtra.h", "upstream/AsstPort.h", "upstream/LICENSE"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		require.NoError(t, err)
		if name == "inputs.json" {
			var manifest map[string]json.RawMessage
			err = json.Unmarshal(data, &manifest)
			require.NoError(t, err)
			manifest["snapshot_id"] = json.RawMessage(`"caller-selected-headers"`)
			data, err = json.Marshal(manifest)
			require.NoError(t, err)
		}
		err = os.MkdirAll(filepath.Dir(filepath.Join(input, name)), 0o755)
		require.NoError(t, err)
		err = os.WriteFile(filepath.Join(input, name), data, 0o644)
		require.NoError(t, err)
	}
	env := environment{Root: root, CInput: input}
	bin, cleanup, err := buildChecker(context.Background(), env)
	require.NoError(t, err, "buildChecker")
	defer cleanup()

	for _, tc := range []struct {
		goos      string
		bound     int
		nonTarget int
	}{
		{goos: "linux", bound: 27, nonTarget: 1},
		{goos: "windows", bound: 28, nonTarget: 0},
	} {
		res := runCheckerTarget(context.Background(), env, bin, tc.goos, tc.bound, tc.nonTarget, 2)
		require.Equal(t, statusPass, res.Status, "checker %s status = %q, want %q (%s)", tc.goos, res.Status, statusPass, res.Detail)
		require.Truef(t, res.HeaderInput != nil && res.HeaderInput.SnapshotID == "caller-selected-headers", "checker did not report the selected input: %+v", res.HeaderInput)
	}
}

func TestCheckerTargetRejectsWrongPolicyCounts(t *testing.T) {
	root, err := resolveRoot("")
	require.NoError(t, err, "resolveRoot")
	env := environment{Root: root, CInput: filepath.Join(root, "tools", "check-maa-api", "testdata", "snapshot")}
	bin, cleanup, err := buildChecker(context.Background(), env)
	require.NoError(t, err, "buildChecker")
	defer cleanup()

	res := runCheckerTarget(context.Background(), env, bin, "linux", 26, 1, 2)
	require.Equal(t, statusFail, res.Status, "wrong bound count status = %q, want %q (%s)", res.Status, statusFail, res.Detail)
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
	require.NoError(t, err, "marshal")
	require.NotContains(t, string(data), "/home/private-user", "report leaked the repository root: %s", data)
}

func TestMergeEnvOverridesWithoutDuplicates(t *testing.T) {
	got := mergeEnv([]string{"A=1", "CGO_ENABLED=1", "GOOS=linux"}, "CGO_ENABLED=0", "GOWORK=off")
	want := []string{"A=1", "GOOS=linux", "CGO_ENABLED=0", "GOWORK=off"}
	require.Equal(t, want, got, "mergeEnv = %v, want %v", got, want)
}

func TestDefinitionsAreUniqueAndConsistent(t *testing.T) {
	defs := definitions()
	seen := map[string]bool{}
	for _, def := range defs {
		require.False(t, seen[def.Name], "duplicate layer %q", def.Name)
		seen[def.Name] = true
		require.NotEmpty(t, def.Description, "layer %q has no description", def.Name)
		require.NotNil(t, def.Run, "layer %q has no run function", def.Name)
		for _, req := range def.Requires {
			switch req {
			case "cc", "headers", "core-input":
			default:
				require.Failf(t, "unknown layer requirement", "layer %q has unknown requirement %q", def.Name, req)
			}
		}
	}
}

func TestMissingHeaderInputIsUnverified(t *testing.T) {
	called := false
	res := runLayer(context.Background(), environment{CInput: t.TempDir()}, layer{
		Name: "static", Requires: []string{"headers"},
		Run: func(context.Context, environment) layerResult {
			called = true
			return layerResult{Status: statusPass, Passed: 1}
		},
	})
	require.Truef(t, !called && res.Status == statusUnverified && res.Passed == 0, "missing header manifest = %+v, called=%v", res, called)
}

func TestCoreInputPresenceAndRequirements(t *testing.T) {
	tests := []struct {
		name, tarball, library, resources, kind string
		unverified                              bool
	}{
		{name: "missing", unverified: true},
		{name: "partial library", library: "/missing/library", kind: "incomplete", unverified: true},
		{name: "partial resources", resources: "/missing/resources", kind: "incomplete", unverified: true},
		{name: "explicit tarball", tarball: "/missing/archive", kind: "tarball"},
		{name: "explicit installation", library: "/missing/library", resources: "/missing/resources", kind: "installation"},
		{name: "conflicting", tarball: "/archive", library: "/library", kind: "conflicting"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind := coreInputKind(tc.tarball, tc.library, tc.resources)
			require.Equal(t, tc.kind, kind, "input kind = %q, want %q", kind, tc.kind)
			err := requirementError("core-input", environment{CoreInput: kind})
			require.Equal(t, tc.unverified, err != nil, "requirement error = %v; unverified=%v", err, tc.unverified)
		})
	}
}

func TestCoreExpectationsPresence(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   bool
	}{
		{values: []string{"MAA_CORE_EXPECTED_VERSION="}},
		{values: []string{"MAA_CORE_LIB_PATH=/library"}},
		{values: []string{"MAA_CORE_EXPECTED_VERSION=caller-version"}, want: true},
		{values: []string{"MAA_CORE_EXPECTED_LIBRARY_SHA256=invalid"}, want: true},
		{values: []string{"MAA_CORE_EXPECTED_TARBALL_SHA256=invalid"}, want: true},
		{values: []string{"MAA_CORE_EXPECTED_VERSION_SHA256=invalid"}, want: true},
	} {
		got := hasCoreExpectations(tc.values)
		require.Equal(t, tc.want, got, "hasCoreExpectations(%v) = %v, want %v", tc.values, got, tc.want)
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
