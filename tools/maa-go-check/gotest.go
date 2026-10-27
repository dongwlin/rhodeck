// Go test and command execution for the layered checker. The parser
// deliberately classifies a skip as UNVERIFIED unless the layer declares the
// exact test as an expected skip; an empty selection is also UNVERIFIED.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// noCGO is the environment for production-facing Go checks. The race layer
// overrides CGO_ENABLED=1 explicitly and is reported separately.
var noCGO = []string{"CGO_ENABLED=0", "GOWORK=off"}

type goTestStep struct {
	Name       string
	Dir        string
	Packages   []string
	Run        string
	Skip       string
	AllowSkips []string
	Race       bool
	Env        []string
	Timeout    time.Duration
}

type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// goTestArgs builds the exact `go test` argv. -race is emitted only when the
// step asks for it; the race layer must request it explicitly or its PASS
// would be a false green.
func goTestArgs(spec goTestStep) []string {
	args := []string{"test", "-json", "-count=1"}
	if spec.Race {
		args = append(args, "-race")
	}
	if spec.Run != "" {
		args = append(args, "-run", spec.Run)
	}
	if spec.Skip != "" {
		args = append(args, "-skip", spec.Skip)
	}
	if spec.Timeout > 0 {
		args = append(args, "-timeout", spec.Timeout.String())
	}
	return append(args, spec.Packages...)
}

// runGoTest runs one `go test -json` invocation and converts its events into
// a layer result. A test-level skip never increments Passed. Subtests are
// tracked separately so a skipped subtest cannot hide behind a passing parent.
func runGoTest(ctx context.Context, env environment, spec goTestStep) layerResult {
	res := newLayerResult(spec.Name)
	args := goTestArgs(spec)

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout+2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cctx, "go", args...)
	cmd.Dir = rootDir(env, spec.Dir)
	cmd.Env = mergeEnv(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	cmd.Env = mergeEnv(cmd.Env, spec.Env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Status = statusFail
		res.Failed = 1
		res.Failures = []string{fmt.Sprintf("%s: stdout pipe: %v", spec.Name, err)}
		return res
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		res.Status = statusFail
		res.Failed = 1
		res.Failures = []string{fmt.Sprintf("%s: start: %v", spec.Name, err)}
		return res
	}

	passed := map[string]bool{}
	failed := map[string]bool{}
	topSkipped := map[string]string{}
	subSkipped := map[string]string{}
	lastOutput := map[string]string{}
	packageFailed := map[string]bool{}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var scanErr error
	for scanner.Scan() {
		var ev testEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue // non-JSON lines belong to stderr/command diagnostics
		}
		switch ev.Action {
		case "output":
			if ev.Test == "" {
				continue
			}
			line := strings.TrimSpace(ev.Output)
			if line == "" || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "===") {
				continue
			}
			lastOutput[ev.Test] = line
		case "pass":
			if ev.Test != "" && !strings.Contains(ev.Test, "/") {
				passed[ev.Test] = true
			}
		case "fail":
			if ev.Test != "" {
				failed[ev.Test] = true
			} else if ev.Package != "" {
				packageFailed[ev.Package] = true
			}
		case "skip":
			if ev.Test == "" {
				continue
			}
			if strings.Contains(ev.Test, "/") {
				subSkipped[ev.Test] = lastOutput[ev.Test]
			} else {
				topSkipped[ev.Test] = lastOutput[ev.Test]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		scanErr = err
	}
	waitErr := cmd.Wait()

	res.Passed = len(passed)
	res.Failed = len(failed)

	allowed := make(map[string]bool, len(spec.AllowSkips))
	for _, name := range spec.AllowSkips {
		allowed[name] = true
	}
	var expected, unexpected []string
	for name, reason := range topSkipped {
		label := skipLabel(name, reason)
		if allowed[name] {
			expected = append(expected, label)
		} else {
			unexpected = append(unexpected, label)
		}
	}
	for name, reason := range subSkipped {
		unexpected = append(unexpected, skipLabel(name, reason))
	}
	sort.Strings(expected)
	sort.Strings(unexpected)
	res.ExpectedSkips = expected
	res.UnexpectedSkips = unexpected
	res.Skipped = len(expected) + len(unexpected)

	var failures []string
	for name := range failed {
		failures = append(failures, name)
	}
	if len(failures) == 0 {
		for pkg := range packageFailed {
			failures = append(failures, "package "+pkg)
		}
	}
	if waitErr != nil && len(failures) == 0 {
		failures = append(failures, fmt.Sprintf("go test: %v", waitErr))
	}
	sort.Strings(failures)
	res.Failures = failures

	if scanErr != nil {
		res.Failures = append(res.Failures, "reading go test output: "+scanErr.Error())
	}

	var parts []string
	switch {
	case len(failures) > 0 || waitErr != nil || scanErr != nil:
		res.Status = statusFail
		if cctx.Err() == context.DeadlineExceeded {
			parts = append(parts, "timed out after "+timeout.String())
		}
	case len(unexpected) > 0:
		res.Status = statusUnverified
	case len(passed) == 0 && len(expected) == 0:
		res.Status = statusUnverified
		parts = append(parts, "no tests matched; an empty selection is not a pass")
	case len(expected) > 0:
		res.Status = statusPassExpected
	default:
		res.Status = statusPass
	}

	if res.Passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", res.Passed))
	}
	if len(expected) > 0 {
		parts = append(parts, "expected skips: "+strings.Join(expected, "; "))
	}
	if len(unexpected) > 0 {
		parts = append(parts, "unexpected skips: "+strings.Join(unexpected, "; "))
	}
	if len(failures) > 0 {
		parts = append(parts, "failures: "+strings.Join(failures, "; "))
	}
	if stderr.Len() > 0 && (res.Status == statusFail || res.Status == statusUnverified) {
		parts = append(parts, "stderr: "+tail(stderr.String(), 5))
	}
	res.Detail = redact(strings.Join(parts, "; "))
	return res
}

func skipLabel(name, reason string) string {
	if reason == "" {
		return name
	}
	return name + ": " + reason
}

func rootDir(env environment, dir string) string {
	if dir == "" {
		return env.Root
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(env.Root, dir)
}

// runCommand runs a non-Go-test command. For gofmt, any output is a failure;
// formatting problems must be visible rather than printed and ignored.
func runCommand(ctx context.Context, env environment, name string, args []string, envVars []string, timeout time.Duration, failOnOutput bool) layerResult {
	res := newLayerResult(name)
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = env.Root
	cmd.Env = mergeEnv(os.Environ(), envVars...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		res.Status = statusFail
		res.Failed = 1
		res.Failures = []string{fmt.Sprintf("%s: %v", name, err)}
		detail := name
		if cctx.Err() == context.DeadlineExceeded {
			detail += " timed out after " + timeout.String()
		}
		if len(bytes.TrimSpace(out)) > 0 {
			detail += ": " + tail(string(out), 8)
		}
		res.Detail = redact(detail)
		return res
	}
	if failOnOutput {
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			res.Status = statusFail
			res.Failed = 1
			res.Failures = []string{name + " reported output"}
			res.Detail = redact(name + " reported output: " + strings.Join(firstLines(trimmed, 20), ", "))
			return res
		}
	}
	res.Passed = 1
	res.Detail = name
	return res
}

func mergeEnv(base []string, overrides ...string) []string {
	if len(overrides) == 0 {
		return base
	}
	keys := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		key, _, _ := strings.Cut(kv, "=")
		keys[key] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if !keys[key] {
			out = append(out, kv)
		}
	}
	return append(out, overrides...)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func firstLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return lines
}

func redact(s string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}
