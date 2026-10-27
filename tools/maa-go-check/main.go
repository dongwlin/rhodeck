// Command maa-go-check runs the layered verification entry for maa-go and
// reports every layer separately.
//
// The command exists to keep "a skip is not a pass" executable: a selected
// layer that skips, matches no tests, or lacks an external resource is
// reported as UNVERIFIED and makes the process exit 2. A selected layer
// with a real failure exits 1. Only layers where every required check
// actually ran and passed produce exit 0.
//
// The CI selection (--layers ci) contains only layers that can run on a
// clean Linux amd64 machine with a C compiler and the vendored checker
// input. Real MaaCore (--layers core) and real-device (--layers device)
// layers are local-only: they need the locked release tarball and an
// explicit device configuration, and CI never claims them.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// coreProfile is the only MaaCore release profile locked by this repository.
const coreProfile = "maa-v6.20.1"

// Layer status values. Statuses are deliberately not booleans: UNVERIFIED
// exists so a missing toolchain, native library, or explicit device
// configuration can never be rendered as a pass.
const (
	statusPass         = "PASS"
	statusPassExpected = "PASS (expected skips)"
	statusUnverified   = "UNVERIFIED"
	statusFail         = "FAIL"
	statusNotSelected  = "NOT SELECTED"
)

// layerResult is the per-layer report. Passed counts executed checks (a
// top-level Go test or a command step); Skipped counts tests that never ran
// their assertions; Failed counts failed tests or commands.
type layerResult struct {
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	Detail          string   `json:"detail,omitempty"`
	Passed          int      `json:"passed"`
	Skipped         int      `json:"skipped"`
	Failed          int      `json:"failed"`
	ExpectedSkips   []string `json:"expected_skips,omitempty"`
	UnexpectedSkips []string `json:"unexpected_skips,omitempty"`
	Failures        []string `json:"failures,omitempty"`
}

func newLayerResult(name string) layerResult {
	return layerResult{Name: name, Status: statusPass}
}

// merge folds one check into a layer aggregate. FAIL dominates UNVERIFIED;
// UNVERIFIED dominates PASS. Detail strings are concatenated for the
// summary table.
func (r *layerResult) merge(other layerResult) {
	r.Passed += other.Passed
	r.Skipped += other.Skipped
	r.Failed += other.Failed
	r.ExpectedSkips = append(r.ExpectedSkips, other.ExpectedSkips...)
	r.UnexpectedSkips = append(r.UnexpectedSkips, other.UnexpectedSkips...)
	r.Failures = append(r.Failures, other.Failures...)
	switch other.Status {
	case statusFail:
		r.Status = statusFail
	case statusUnverified:
		if r.Status != statusFail {
			r.Status = statusUnverified
		}
	case statusPassExpected:
		if r.Status == statusPass {
			r.Status = statusPassExpected
		}
	}
	if other.Detail != "" {
		if r.Detail != "" {
			r.Detail += "; "
		}
		r.Detail += other.Detail
	}
}

// environment is the runtime context shared by layer definitions. Root is
// intentionally not serialized: reports must not contain absolute home
// paths or repository-external locations.
type environment struct {
	Root         string
	GoVersion    string
	GOOS         string
	GOARCH       string
	Purego       string
	CGOEnabled   string
	CC           string
	CCVersion    string
	ADB          string
	CoreSHA256   string
	DeviceConfig bool
	GitCommit    string
}

type envReport struct {
	GoVersion    string `json:"go_version"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
	Purego       string `json:"purego"`
	CGOEnv       string `json:"cgo_env"`
	CC           string `json:"cc"`
	CCVersion    string `json:"cc_version"`
	ADB          string `json:"adb"`
	CoreProfile  string `json:"core_profile"`
	CoreSHA256   string `json:"core_tarball_sha256"`
	DeviceConfig bool   `json:"device_config_set"`
	GitCommit    string `json:"git_commit"`
}

type report struct {
	GeneratedAt string        `json:"generated_at"`
	Environment envReport     `json:"environment"`
	Selected    []string      `json:"selected"`
	Layers      []layerResult `json:"layers"`
	Conclusion  string        `json:"conclusion"`
}

func main() {
	var layersFlag, rootFlag string
	var list, jsonOut bool
	flag.StringVar(&layersFlag, "layers", "ci", "comma-separated layers or aliases: ci, all, static, go, device-gate, loader, abi, callback, core, device, build, fmt, race")
	flag.StringVar(&rootFlag, "root", "", "repository root (default: nearest ancestor containing go.mod)")
	flag.BoolVar(&list, "list", false, "list layers and exit")
	flag.BoolVar(&jsonOut, "json", false, "print the machine-readable report instead of the table")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "maa-go-check: unexpected positional arguments: %s\n", strings.Join(flag.Args(), " "))
		os.Exit(2)
	}

	root, err := resolveRoot(rootFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "maa-go-check: %v\n", err)
		os.Exit(2)
	}
	defs := definitions()
	if list {
		printLayerList(defs)
		return
	}
	selected, err := selectLayers(defs, layersFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "maa-go-check: %v\n", err)
		os.Exit(2)
	}

	env := collectEnvironment(root)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	results := make([]layerResult, 0, len(defs))
	for _, def := range defs {
		if !selected[def.Name] {
			results = append(results, layerResult{Name: def.Name, Status: statusNotSelected, Detail: def.Description})
			continue
		}
		if !jsonOut {
			fmt.Printf("==> %s: %s\n", def.Name, def.Description)
		}
		res := runLayer(ctx, env, def)
		results = append(results, res)
		if !jsonOut {
			fmt.Printf("    %s (%d passed, %d skipped, %d failed)\n", res.Status, res.Passed, res.Skipped, res.Failed)
		}
	}

	rep := buildReport(env, selected, results)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "maa-go-check: encode report: %v\n", err)
			os.Exit(2)
		}
	} else {
		printSummary(rep)
	}
	os.Exit(exitCode(rep))
}

// runLayer checks external requirements before invoking a layer. A missing
// requirement is UNVERIFIED, never a skip that could be mistaken for a pass.
func runLayer(ctx context.Context, env environment, def layer) layerResult {
	for _, req := range def.Requires {
		if err := requirementError(req, env); err != nil {
			return layerResult{Name: def.Name, Status: statusUnverified, Detail: err.Error()}
		}
	}
	res := def.Run(ctx, env)
	res.Name = def.Name
	if res.Status == "" {
		res.Status = statusPass
	}
	return res
}

func requirementError(name string, env environment) error {
	switch name {
	case "cc":
		if env.CC == "" {
			return errors.New("C compiler cc/gcc not found; C fixture layer is unverified")
		}
	case "tarball":
		if env.CoreSHA256 == "" {
			return errors.New("locked MaaCore tarball missing (set MAA_M1_TARBALL or place MAA-v6.20.1-linux-x86_64.tar.gz in the user cache); real-core layer is unverified")
		}
	case "device-config":
		if !env.DeviceConfig {
			return errors.New("MAA_M4_DEVICE_CONFIG is not set; device scenarios are explicit-only and unverified")
		}
	default:
		return fmt.Errorf("unknown layer requirement %q", name)
	}
	return nil
}

func resolveRoot(flagRoot string) (string, error) {
	if flagRoot != "" {
		abs, err := filepath.Abs(flagRoot)
		if err != nil {
			return "", fmt.Errorf("resolve root %s: %w", flagRoot, err)
		}
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
			return "", fmt.Errorf("root %s does not contain go.mod: %w", abs, err)
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found in the working directory or any ancestor")
		}
		dir = parent
	}
}

func selectLayers(defs []layer, spec string) (map[string]bool, error) {
	known := make(map[string]bool, len(defs))
	all := make([]string, 0, len(defs))
	for _, def := range defs {
		known[def.Name] = true
		all = append(all, def.Name)
	}
	sort.Strings(all)
	aliases := map[string][]string{
		"ci":  {"static", "go", "device-gate", "loader", "abi", "callback", "build", "fmt"},
		"all": all,
	}
	if strings.TrimSpace(spec) == "" {
		spec = "ci"
	}
	selected := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if names, ok := aliases[part]; ok {
			for _, name := range names {
				selected[name] = true
			}
			continue
		}
		if !known[part] {
			return nil, fmt.Errorf("unknown layer %q (use --list)", part)
		}
		selected[part] = true
	}
	if len(selected) == 0 {
		return nil, errors.New("no layers selected")
	}
	return selected, nil
}

func printLayerList(defs []layer) {
	fmt.Println("maa-go layered verification")
	fmt.Println()
	fmt.Printf("%-14s %-46s %s\n", "LAYER", "DESCRIPTION", "REQUIRES")
	for _, def := range defs {
		req := strings.Join(def.Requires, ",")
		if req == "" {
			req = "-"
		}
		fmt.Printf("%-14s %-46s %s\n", def.Name, def.Description, req)
	}
	fmt.Println()
	fmt.Println("aliases: ci = static,go,device-gate,loader,abi,callback,build,fmt")
	fmt.Println("         all = every layer (real MaaCore and device layers need local resources/authorization)")
	fmt.Println("local-only layers: core, device (never run or claimed by CI)")
}

func collectEnvironment(root string) environment {
	env := environment{
		Root:         root,
		GoVersion:    goEnv(root, "GOVERSION"),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		Purego:       puregoVersion(root),
		CGOEnabled:   goEnv(root, "CGO_ENABLED"),
		CC:           firstLookPath("cc", "gcc"),
		ADB:          firstLookPath("adb"),
		DeviceConfig: os.Getenv("MAA_M4_DEVICE_CONFIG") != "",
		GitCommit:    gitCommit(root),
	}
	if env.GoVersion == "" {
		env.GoVersion = runtime.Version()
	}
	if env.CGOEnabled == "" {
		env.CGOEnabled = os.Getenv("CGO_ENABLED")
	}
	env.CCVersion = ccVersion(env.CC)
	if sha, ok := coreTarballSHA256(); ok {
		env.CoreSHA256 = sha
	}
	return env
}

func goEnv(dir, name string) string {
	cmd := exec.Command("go", "env", name)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func puregoVersion(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "github.com/ebitengine/purego" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}
	return ""
}

func firstLookPath(names ...string) string {
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			return filepath.Base(path)
		}
	}
	return ""
}

func coreTarballSHA256() (string, bool) {
	path := os.Getenv("MAA_M1_TARBALL")
	if path == "" {
		path = os.Getenv("CHECK_MAA_API_TARBALL")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		path = filepath.Join(home, ".cache", "maa", "MAA-v6.20.1-linux-x86_64.tar.gz")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func ccVersion(name string) string {
	if name == "" {
		return ""
	}
	cmd := exec.Command(name, "--version")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return redact(line)
}

func gitCommit(root string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func buildReport(env environment, selected map[string]bool, results []layerResult) report {
	selectedNames := make([]string, 0, len(selected))
	for name := range selected {
		selectedNames = append(selectedNames, name)
	}
	sort.Strings(selectedNames)
	return report{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Environment: envReport{
			GoVersion:    env.GoVersion,
			GOOS:         env.GOOS,
			GOARCH:       env.GOARCH,
			Purego:       env.Purego,
			CGOEnv:       env.CGOEnabled,
			CC:           env.CC,
			CCVersion:    env.CCVersion,
			ADB:          env.ADB,
			CoreProfile:  coreProfile,
			CoreSHA256:   env.CoreSHA256,
			DeviceConfig: env.DeviceConfig,
			GitCommit:    env.GitCommit,
		},
		Selected:   selectedNames,
		Layers:     results,
		Conclusion: conclusion(selected, results),
	}
}

func conclusion(selected map[string]bool, results []layerResult) string {
	var failed, unverified, expected []string
	for _, name := range sortedKeys(selected) {
		res := findLayer(results, name)
		if res == nil {
			failed = append(failed, name+" (missing result)")
			continue
		}
		switch res.Status {
		case statusFail:
			failed = append(failed, name)
		case statusUnverified:
			unverified = append(unverified, name)
		case statusPassExpected:
			expected = append(expected, name)
		}
	}
	if len(failed) > 0 {
		return "FAIL: " + strings.Join(failed, ", ") + " failed; fix the failures and re-run"
	}
	if len(unverified) > 0 {
		return "UNVERIFIED: " + strings.Join(unverified, ", ") + " did not run to completion; a skip or missing resource is not a pass"
	}
	if len(expected) > 0 {
		return "all selected layers completed; expected skips in " + strings.Join(expected, ", ") + " did not verify the skipped behavior"
	}
	return "all selected layers verified: " + strings.Join(sortedKeys(selected), ", ")
}

func findLayer(results []layerResult, name string) *layerResult {
	for i := range results {
		if results[i].Name == name {
			return &results[i]
		}
	}
	return nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func exitCode(rep report) int {
	if len(rep.Selected) == 0 {
		return 2
	}
	code := 0
	for _, name := range rep.Selected {
		res := findLayer(rep.Layers, name)
		if res == nil {
			return 1
		}
		switch res.Status {
		case statusFail, statusNotSelected:
			return 1
		case statusUnverified:
			code = 2
		}
	}
	return code
}

func printSummary(rep report) {
	fmt.Println()
	fmt.Println("maa-go layered verification")
	fmt.Printf("environment: go=%s %s/%s purego=%s cgo_env=%s cc=%s adb=%s core_sha256=%s device_config=%s git=%s\n",
		rep.Environment.GoVersion,
		rep.Environment.GOOS, rep.Environment.GOARCH,
		orAbsent(rep.Environment.Purego),
		orAbsent(rep.Environment.CGOEnv),
		orAbsent(rep.Environment.CC),
		orAbsent(rep.Environment.ADB),
		shortSHA(rep.Environment.CoreSHA256),
		setWord(rep.Environment.DeviceConfig),
		orAbsent(rep.Environment.GitCommit),
	)
	if rep.Environment.CCVersion != "" {
		fmt.Println("c compiler:", rep.Environment.CCVersion)
	}
	fmt.Printf("%-14s %-22s %5s %5s %5s  %s\n", "LAYER", "STATUS", "PASS", "SKIP", "FAIL", "DETAIL")
	for _, res := range rep.Layers {
		fmt.Printf("%-14s %-22s %5d %5d %5d  %s\n", res.Name, res.Status, res.Passed, res.Skipped, res.Failed, truncate(res.Detail, 120))
	}
	fmt.Println()
	fmt.Println("conclusion:", rep.Conclusion)
	if hasStatus(rep, statusUnverified) {
		fmt.Println("note: UNVERIFIED is not a pass; provide the missing toolchain/resource/authorization and re-run the layer.")
	}
	if hasStatus(rep, statusPassExpected) {
		fmt.Println("note: PASS (expected skips) lists skips that are part of the asserted behavior; the skipped tests did not verify anything.")
	}
	if hasStatus(rep, statusNotSelected) {
		fmt.Println("note: NOT SELECTED layers were not run and are not claimed by this invocation.")
	}
}

func hasStatus(rep report, status string) bool {
	for _, res := range rep.Layers {
		if res.Status == status {
			return true
		}
	}
	return false
}

func orAbsent(s string) string {
	if s == "" {
		return "absent"
	}
	return s
}

func shortSHA(s string) string {
	if s == "" {
		return "absent"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func setWord(b bool) string {
	if b {
		return "set"
	}
	return "unset"
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
