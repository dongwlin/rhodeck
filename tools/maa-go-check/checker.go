// Static checker execution: build the check-maa-api binary once and run it
// against the selected, provenance-backed C input for both targets.
// The parsed report is checked for the expected bound/excluded/signature
// counts so a no-op or empty run cannot be reported as a pass.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// checkerReport is the subset of check-maa-api's JSON output used by the
// layered entry. The checker itself owns the full schema.
type checkerReport struct {
	Input  checkerInputReport `json:"input"`
	Target struct {
		GOOS   string `json:"goos"`
		GOARCH string `json:"goarch"`
	} `json:"target"`
	Link struct {
		Bound     []json.RawMessage `json:"bound"`
		NonTarget []string          `json:"nontarget_bindings"`
		Excluded  []string          `json:"excluded"`
	} `json:"link"`
	Signatures struct {
		Symbols   []json.RawMessage `json:"symbols"`
		Callbacks []json.RawMessage `json:"callbacks"`
	} `json:"signatures"`
	Expect struct {
		Symbols []json.RawMessage `json:"symbols"`
	} `json:"expect"`
}

// The checker validates provenance and actual file digests. Retain that
// evidence in the layer report without including the input's local path.
type checkerInputReport struct {
	SchemaVersion int               `json:"schema_version"`
	SnapshotID    string            `json:"snapshot_id"`
	Source        json.RawMessage   `json:"source"`
	Files         []json.RawMessage `json:"files"`
}

// buildChecker compiles the checker into a temporary directory. The caller
// must invoke the returned cleanup function.
func buildChecker(ctx context.Context, env environment) (string, func(), error) {
	dir, err := os.MkdirTemp("", "maa-go-check-*")
	if err != nil {
		return "", nil, fmt.Errorf("create checker temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	bin := filepath.Join(dir, "check-maa-api")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "build", "-o", bin, "./tools/check-maa-api")
	cmd.Dir = env.Root
	cmd.Env = mergeEnv(os.Environ(), noCGO...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("build checker: %w: %s", err, tail(string(out), 5))
	}
	return bin, cleanup, nil
}

// runCheckerTarget runs the built checker for one static target and verifies
// the policy counts that make the output meaningful. A zero-test or
// wrong-policy run is a failure, not an unverified skip.
func runCheckerTarget(ctx context.Context, env environment, bin, goos string, bound, nonTarget, excluded int) layerResult {
	res := newLayerResult("static/" + goos)
	args := []string{
		bin,
		"--goos", goos,
		"--goarch", "amd64",
		"--c-input", env.CInput,
		"--go-root", env.Root,
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, args[0], args[1:]...)
	cmd.Dir = env.Root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		res.Status = statusFail
		res.Failed = 1
		res.Failures = []string{fmt.Sprintf("checker %s: %v", goos, err)}
		res.Detail = redact(fmt.Sprintf("checker %s failed: %v; %s", goos, err, tail(stderr.String(), 5)))
		return res
	}
	var rep checkerReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		res.Status = statusFail
		res.Failed = 1
		res.Failures = []string{fmt.Sprintf("checker %s: decode report", goos)}
		res.Detail = redact(fmt.Sprintf("checker %s report is not JSON: %v", goos, err))
		return res
	}
	var problems []string
	if rep.Input.SchemaVersion != 1 || rep.Input.SnapshotID == "" || len(rep.Input.Files) < 3 || len(rep.Input.Source) == 0 || bytes.Equal(rep.Input.Source, []byte("null")) {
		problems = append(problems, "missing header input provenance or file digests")
	}
	if rep.Target.GOOS != goos || rep.Target.GOARCH != "amd64" {
		problems = append(problems, fmt.Sprintf("target=%s/%s want %s/amd64", rep.Target.GOOS, rep.Target.GOARCH, goos))
	}
	if len(rep.Link.Bound) != bound {
		problems = append(problems, fmt.Sprintf("bound=%d want %d", len(rep.Link.Bound), bound))
	}
	if len(rep.Link.NonTarget) != nonTarget {
		problems = append(problems, fmt.Sprintf("nontarget_bindings=%d want %d", len(rep.Link.NonTarget), nonTarget))
	}
	if len(rep.Link.Excluded) != excluded {
		problems = append(problems, fmt.Sprintf("excluded=%d want %d", len(rep.Link.Excluded), excluded))
	}
	if len(rep.Signatures.Symbols) != 28 {
		problems = append(problems, fmt.Sprintf("signature symbols=%d want 28", len(rep.Signatures.Symbols)))
	}
	if len(rep.Signatures.Callbacks) != 2 {
		problems = append(problems, fmt.Sprintf("signature callbacks=%d want 2", len(rep.Signatures.Callbacks)))
	}
	if len(rep.Expect.Symbols) != 30 {
		problems = append(problems, fmt.Sprintf("expect symbols=%d want 30", len(rep.Expect.Symbols)))
	}
	if len(problems) > 0 {
		res.Status = statusFail
		res.Failed = 1
		res.Failures = append(res.Failures, problems...)
		res.Detail = fmt.Sprintf("checker %s policy mismatch: %s", goos, joinProblems(problems))
		return res
	}
	res.Passed = 1
	res.HeaderInput = &rep.Input
	res.Detail = fmt.Sprintf("%s: snapshot_id=%s bound=%d nontarget=%d excluded=%d signatures=%d/%d expect=%d",
		goos, rep.Input.SnapshotID, len(rep.Link.Bound), len(rep.Link.NonTarget), len(rep.Link.Excluded),
		len(rep.Signatures.Symbols), len(rep.Signatures.Callbacks), len(rep.Expect.Symbols))
	return res
}

func joinProblems(problems []string) string {
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
