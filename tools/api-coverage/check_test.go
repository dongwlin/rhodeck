package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// repoRoot locates the repository root from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err, "locate repo root")
	return root
}

// copyGoTree copies every .go file under src into dst, preserving the
// relative structure. Only main-project Go files matter for the checker, so
// the copy is deliberately shallow in content.
func copyGoTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	require.NoError(t, err, "copy go tree")
}

// mutatedRepo copies the repository into a temp directory, applies replace
// (old, new) pairs to the named binding files, and returns the temp root.
func mutatedRepo(t *testing.T, edits map[string][2]string) string {
	t.Helper()
	src := repoRoot(t)
	dst := t.TempDir()
	copyGoTree(t, src, dst)
	for file, edit := range edits {
		path := filepath.Join(dst, file)
		data, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", file)
		require.Contains(t, string(data), edit[0], "mutation target %q not found in %s", edit[0], file)
		data = []byte(strings.Replace(string(data), edit[0], edit[1], 1))
		require.NoError(t, os.WriteFile(path, data, 0o644), "write %s", file)
	}
	return dst
}

func scanAndCheck(t *testing.T, root string, mode Mode) *Report {
	t.Helper()
	scan, err := ScanBinding(root)
	require.NoError(t, err, "scan binding")
	report, err := Check(scan, root, mode)
	require.NoError(t, err, "check")
	return report
}

func hasItemWithPrefix(items []string, prefix string) bool {
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

// TestScanIsReproducible verifies that repeated scans produce the identical
// inventory (the checklist must be reproducibly derivable from source).
func TestScanIsReproducible(t *testing.T) {
	root := repoRoot(t)
	first, err := ScanBinding(root)
	require.NoError(t, err, "first scan")
	second, err := ScanBinding(root)
	require.NoError(t, err, "second scan")
	require.Len(t, second, len(first), "scan produced %d then %d symbols", len(first), len(second))
	for i := range first {
		require.Equal(t, first[i], second[i], "scan not reproducible at %d: %+v vs %+v", i, first[i], second[i])
	}
}

// TestFrozenInventoryReconcilesWithSource verifies the frozen symbol table
// matches the current source exactly in drift mode: no drift and no mapping
// failures. Registered gaps are allowed in drift mode, whatever their count;
// the gap semantics are exercised on a controlled copy in
// TestFinalModeFailsWhileGapsRemain.
func TestFrozenInventoryReconcilesWithSource(t *testing.T) {
	report := scanAndCheck(t, repoRoot(t), ModeDrift)
	require.True(t, report.Pass, "drift check failed: added=%v removed=%v changed=%v mapping=%v",
		report.Added, report.Removed, report.Changed, report.Mapping)
	require.Equal(t, report.Frozen, report.Scanned, "scanned %d items but inventory freezes %d", report.Scanned, report.Frozen)
}

// TestFinalModeFailsWhileGapsRemain verifies the two-mode gap semantics:
// registered gaps do not count as coverage, so final mode fails while they
// exist, and drift mode still passes with them. The live inventory is
// gap-free since WKR-09, so one done entry is downgraded to a registered gap
// in memory instead of depending on the live gap count.
func TestFinalModeFailsWhileGapsRemain(t *testing.T) {
	mappings := mappingsCopy(t)
	downgraded := ""
	for key, m := range mappings {
		if m.Status == "done" {
			m.Status = "pending:WKR-12"
			mappings[key] = m
			downgraded = key
			break
		}
	}
	require.NotEmpty(t, downgraded, "no done entry to downgrade into a registered gap")
	root := repoRoot(t)
	drift := checkWithInventory(t, root, mappings, ModeDrift)
	require.True(t, drift.Pass, "drift check must pass while registered gaps exist: added=%v removed=%v changed=%v mapping=%v",
		drift.Added, drift.Removed, drift.Changed, drift.Mapping)
	report := checkWithInventory(t, root, mappings, ModeFinal)
	require.False(t, report.Pass, "final check passed while registered gaps remain; gaps must not count as coverage")
	require.NotZero(t, report.Pending, "final report lists no pending gaps although the inventory carries them")
	found := false
	for _, f := range report.Mapping {
		if strings.Contains(f.Problem, "zero pending items") {
			found = true
		}
	}
	require.True(t, found, "final report does not state the zero-pending requirement")
	for _, gap := range report.Gaps {
		require.Contains(t, gap, "pending:WKR-", "gap %q is not attributed to a WKR task", gap)
	}
}

// TestMutationAddedMethod: a new exported method without an inventory entry
// fails the drift check.
func TestMutationAddedMethod(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/runtime.go": {"func Open(cfg RuntimeConfig) (*Runtime, error) {",
			"func (rt *Runtime) ExtraAPI() string { return \"\" }\n\nfunc Open(cfg RuntimeConfig) (*Runtime, error) {"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "adding an exported method must fail the drift check")
	require.True(t, hasItemWithPrefix(report.Added, "entry Runtime.ExtraAPI"), "added method not reported: %v", report.Added)
}

// TestMutationAddedLimitsField: a new Limits field is a field-level drift,
// not just a method-level one.
func TestMutationAddedLimitsField(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/runtime.go": {"type Limits struct {\n", "type Limits struct {\n\tExtra uint64\n"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "adding a Limits field must fail the drift check")
	require.True(t, hasItemWithPrefix(report.Added, "field Limits.Extra"), "added field not reported: %v", report.Added)
}

// TestMutationChangedSignature: reshaping a frozen signature fails.
func TestMutationChangedSignature(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/runtime.go": {"func (rt *Runtime) Version() string { return rt.version }",
			"func (rt *Runtime) Version() (string, error) { return rt.version, nil }"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "changing a frozen signature must fail the drift check")
	require.True(t, len(report.Changed) == 1 && strings.HasPrefix(report.Changed[0], "entry Runtime.Version"), "changed signature not reported: %v", report.Changed)
}

// TestMutationRenamedMethod: deleting or renaming a method removes a frozen
// entry and adds an unregistered one.
func TestMutationRenamedMethod(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/instance.go": {"func (in *Instance) UUID() (string, error)", "func (in *Instance) UUID2() (string, error)"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "renaming an entry must fail the drift check")
	require.True(t, hasItemWithPrefix(report.Removed, "entry Instance.UUID "), "removed entry not reported: %v", report.Removed)
	require.True(t, hasItemWithPrefix(report.Added, "entry Instance.UUID2"), "added entry not reported: %v", report.Added)
}

// TestMutationAddedConstant: a new constant (e.g. another Win32 input method)
// cannot appear without registration.
func TestMutationAddedConstant(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/window.go": {"type Win32InputMethod uint64",
			"type Win32InputMethod uint64\n\nconst Win32InputNew Win32InputMethod = 1 << 11"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "adding a constant must fail the drift check")
	require.True(t, hasItemWithPrefix(report.Added, "const Win32InputNew"), "added constant not reported: %v", report.Added)
}

// TestMutationRenamedErrorSentinel: the error surface is part of the
// inventory; renaming a sentinel drifts.
func TestMutationRenamedErrorSentinel(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/errors.go": {"ErrProtocol = native.ErrProtocol", "ErrProtocol2 = native.ErrProtocol"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "renaming an error sentinel must fail the drift check")
	require.True(t, hasItemWithPrefix(report.Removed, "error ErrProtocol "), "removed sentinel not reported: %v", report.Removed)
	require.True(t, hasItemWithPrefix(report.Added, "error ErrProtocol2"), "added sentinel not reported: %v", report.Added)
}

// TestMutationChangedFieldType: changing a field's type drifts at field
// level.
func TestMutationChangedFieldType(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/maa-go/runtime.go": {"GrowthRetries uint32", "GrowthRetries int32"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "changing a field type must fail the drift check")
	require.True(t, len(report.Changed) == 1 && strings.HasPrefix(report.Changed[0], "field BufferLimits.GrowthRetries"), "changed field not reported: %v", report.Changed)
}

func mappingsCopy(t *testing.T) map[string]Mapping {
	t.Helper()
	src := inventoryMappings()
	out := make(map[string]Mapping, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func checkWithInventory(t *testing.T, root string, mappings map[string]Mapping, mode Mode) *Report {
	t.Helper()
	scan, err := ScanBinding(root)
	require.NoError(t, err, "scan")
	frozen, err := inventorySymbols()
	require.NoError(t, err, "inventory")
	report, err := checkWith(scan, frozen, mappings, root, mode)
	require.NoError(t, err, "check")
	return report
}

// TestCapabilityMarkerAloneCannotSatisfyCheck: marking a pending item done
// with only a capability flag in the DTO segment still fails — the worker,
// client, and test segments are mandatory.
func TestCapabilityMarkerAloneCannotSatisfyCheck(t *testing.T) {
	mappings := mappingsCopy(t)
	m := mappings["entry Runtime.Log"]
	m.Status = "done"
	m.DTO = "Capabilities.ImageBGR"
	m.Worker, m.Client, m.Test = "", "", ""
	mappings["entry Runtime.Log"] = m
	report := checkWithInventory(t, repoRoot(t), mappings, ModeFinal)
	require.False(t, report.Pass, "a capability marker alone must not satisfy the check")
	problems := 0
	for _, f := range report.Mapping {
		if f.Item == "entry Runtime.Log" && strings.Contains(f.Problem, "empty") {
			problems++
		}
	}
	require.Equal(t, 3, problems, "expected three empty-segment failures, got %d: %v", problems, report.Mapping)
}

// TestUnresolvableTestReferenceFails: acceptance mappings must point at real
// test functions.
func TestUnresolvableTestReferenceFails(t *testing.T) {
	mappings := mappingsCopy(t)
	m := mappings["entry Open"]
	m.Test = "TestNoSuchTestEver"
	mappings["entry Open"] = m
	report := checkWithInventory(t, repoRoot(t), mappings, ModeDrift)
	require.False(t, report.Pass, "a fabricated test reference must fail the check")
	found := false
	for _, f := range report.Mapping {
		if f.Item == "entry Open" && strings.Contains(f.Problem, `test reference "TestNoSuchTestEver" does not resolve`) {
			found = true
		}
	}
	require.True(t, found, "unresolved test reference not reported: %v", report.Mapping)
}

// TestRemovedWorkerBranchFails: dropping the worker adaptation (e.g. renaming
// the engine method away) breaks the verified mapping.
func TestRemovedWorkerBranchFails(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"apps/worker/internal/app/backend.go": {"func (e *nativeEngine) AppendTask(req protocol.TaskRequest) (int32, error) {",
			"func (e *nativeEngine) AppendTaskX(req protocol.TaskRequest) (int32, error) {"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "losing the worker branch must fail the check")
	found := false
	for _, f := range report.Mapping {
		if f.Item == "entry Instance.AppendTask" && strings.Contains(f.Problem, `worker reference "nativeEngine.AppendTask" does not resolve`) {
			found = true
		}
	}
	require.True(t, found, "missing worker branch not reported: %v", report.Mapping)
}

// TestRemovedClientBranchFails: dropping the client method breaks the
// verified mapping on the client segment.
func TestRemovedClientBranchFails(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/control/worker/client.go": {"func (c *Client) AppendTask(ctx context.Context, req protocol.TaskRequest) (protocol.TaskAccepted, error) {",
			"func (c *Client) AppendTaskX(ctx context.Context, req protocol.TaskRequest) (protocol.TaskAccepted, error) {"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "losing the client method must fail the check")
	found := false
	for _, f := range report.Mapping {
		if f.Item == "entry Instance.AppendTask" && strings.Contains(f.Problem, `client reference "Client.AppendTask" does not resolve`) {
			found = true
		}
	}
	require.True(t, found, "missing client method not reported: %v", report.Mapping)
}

// TestRemovedDTOFails: dropping the protocol DTO breaks the verified mapping
// on the transport segment.
func TestRemovedDTOFails(t *testing.T) {
	root := mutatedRepo(t, map[string][2]string{
		"packages/control/worker/protocol/protocol.go": {"type ConnectRequest struct {", "type ConnectRequestX struct {"},
	})
	report := scanAndCheck(t, root, ModeDrift)
	require.False(t, report.Pass, "losing the protocol DTO must fail the check")
	found := false
	for _, f := range report.Mapping {
		if f.Item == "entry Instance.Connect" && strings.Contains(f.Problem, `dto reference "ConnectRequest.ADBPath" does not resolve`) {
			found = true
		}
	}
	require.True(t, found, "missing DTO not reported: %v", report.Mapping)
}

// TestNAWithoutReasonIsRejected: non-applicable entries must state the reason
// and the equivalent mapping.
func TestNAWithoutReasonIsRejected(t *testing.T) {
	mappings := mappingsCopy(t)
	mappings["capability Capabilities.Create"] = Mapping{Status: "na"}
	scan, err := ScanBinding(repoRoot(t))
	require.NoError(t, err, "scan")
	frozen, err := inventorySymbols()
	require.NoError(t, err, "inventory")
	_, err = checkWith(scan, frozen, mappings, repoRoot(t), ModeDrift)
	require.Error(t, err, "na status without a reason must be rejected")
}

// TestInvalidStatusIsRejected: statuses outside the frozen vocabulary are
// inventory errors, not drift.
func TestInvalidStatusIsRejected(t *testing.T) {
	mappings := mappingsCopy(t)
	m := mappings["entry Open"]
	m.Status = "covered"
	mappings["entry Open"] = m
	scan, err := ScanBinding(repoRoot(t))
	require.NoError(t, err, "scan")
	frozen, err := inventorySymbols()
	require.NoError(t, err, "inventory")
	_, err = checkWith(scan, frozen, mappings, repoRoot(t), ModeDrift)
	require.Error(t, err, "an invalid status must be rejected")
}

// TestMappingWithoutSymbolIsRejected: mappings may not reference symbols the
// inventory does not freeze.
func TestMappingWithoutSymbolIsRejected(t *testing.T) {
	mappings := mappingsCopy(t)
	mappings["entry Runtime.Ghost"] = Mapping{Status: "done", DTO: "Error", Worker: "classify", Client: "Client.Err", Test: "TestNativeErrorCodes"}
	scan, err := ScanBinding(repoRoot(t))
	require.NoError(t, err, "scan")
	frozen, err := inventorySymbols()
	require.NoError(t, err, "inventory")
	_, err = checkWith(scan, frozen, mappings, repoRoot(t), ModeDrift)
	require.Error(t, err, "a mapping without a frozen symbol must be rejected")
}
