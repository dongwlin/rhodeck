package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Mode selects the enforcement strength.
//
//   - drift: the source must match the frozen inventory exactly, and every
//     done entry's mapping references must resolve. Pending gaps are allowed
//     while the full worker implementation tasks are open.
//   - final: additionally, no pending item may remain. Registered gaps do not
//     count as coverage; the final check fails while any entry is pending.
type Mode string

const (
	ModeDrift Mode = "drift"
	ModeFinal Mode = "final"
)

func parseMode(s string) (Mode, error) {
	switch s {
	case string(ModeDrift):
		return ModeDrift, nil
	case string(ModeFinal):
		return ModeFinal, nil
	default:
		return "", fmt.Errorf("unknown mode %q (want drift or final)", s)
	}
}

var pendingTaskPattern = regexp.MustCompile(`^pending:WKR-(0[1-9]|1[0-2])$`)

// Mapping is the five-segment registration of one inventory item:
// binding -> DTO/transport (or equivalent merge) -> worker adaptation ->
// client surface -> acceptance test.
type Mapping struct {
	// DTO names the protocol DTO type, field, or command constant that
	// carries the capability (comma-separated references).
	DTO string
	// Worker names the worker app adaptation identifiers.
	Worker string
	// Client names the supervision client identifiers.
	Client string
	// Test names the acceptance test functions that cover the chain.
	Test string
	// Status is "done", "pending:WKR-xx", or "na".
	Status string
	// Note records equivalent merges and non-applicable reasons. Required
	// for "na" statuses.
	Note string
}

// Finding is one check failure.
type Finding struct {
	Item    string
	Problem string
}

func (f Finding) String() string {
	if f.Item == "" {
		return f.Problem
	}
	return fmt.Sprintf("%s: %s", f.Item, f.Problem)
}

// Report is the check result.
type Report struct {
	Mode    Mode
	Pass    bool
	Scanned int
	Frozen  int
	Done    int
	Pending int
	NA      int
	Added   []string
	Removed []string
	Changed []string
	Mapping []Finding
	Gaps    []string
}

// Check reconciles the scanned surface against the frozen inventory and
// verifies the five-segment mappings. root locates the protocol, worker, and
// client packages for reference resolution.
func Check(scan []Symbol, root string, mode Mode) (*Report, error) {
	frozen, err := inventorySymbols()
	if err != nil {
		return nil, err
	}
	return checkWith(scan, frozen, inventoryMappings(), root, mode)
}

// checkWith is Check with an explicit inventory; tests use it to exercise
// mapping mutations without editing the frozen file.
func checkWith(scan []Symbol, frozen []Symbol, mappings map[string]Mapping, root string, mode Mode) (*Report, error) {
	report := &Report{Mode: mode, Scanned: len(scan), Frozen: len(frozen)}
	scanByKey := map[string]Symbol{}
	for _, s := range scan {
		if prev, dup := scanByKey[s.Key()]; dup {
			return nil, fmt.Errorf("scanner produced duplicate key %q (%q and %q)", s.Key(), prev.Detail, s.Detail)
		}
		scanByKey[s.Key()] = s
	}
	frozenByKey := map[string]Symbol{}
	for _, s := range frozen {
		if prev, dup := frozenByKey[s.Key()]; dup {
			return nil, fmt.Errorf("inventory contains duplicate key %q (%q and %q)", s.Key(), prev.Detail, s.Detail)
		}
		frozenByKey[s.Key()] = s
	}

	// Inventory self-consistency: every frozen symbol needs a mapping and
	// vice versa; statuses must be well-formed.
	for _, key := range keysOf(frozenByKey) {
		m, ok := mappings[key]
		if !ok {
			return nil, fmt.Errorf("inventory mapping missing for %q", key)
		}
		if problem := validateStatus(m); problem != "" {
			return nil, fmt.Errorf("inventory mapping for %q: %s", key, problem)
		}
	}
	for _, key := range keysOf(mappings) {
		if _, ok := frozenByKey[key]; !ok {
			return nil, fmt.Errorf("inventory mapping without a frozen symbol: %q", key)
		}
	}

	// Drift: set equality plus per-item detail equality.
	for _, key := range keysOf(scanByKey) {
		if _, ok := frozenByKey[key]; !ok {
			report.Added = append(report.Added, key+" ("+scanByKey[key].Detail+")")
		}
	}
	for _, key := range keysOf(frozenByKey) {
		s, ok := scanByKey[key]
		if !ok {
			report.Removed = append(report.Removed, key+" ("+frozenByKey[key].Detail+")")
			continue
		}
		if s.Detail != frozenByKey[key].Detail {
			report.Changed = append(report.Changed, fmt.Sprintf("%s: inventory %q vs source %q", key, frozenByKey[key].Detail, s.Detail))
		}
	}

	// Mapping verification against the real tree. Performed even when drift
	// was found, so one run reports everything it can see.
	idx, err := NewRefIndex(root)
	if err != nil {
		return nil, err
	}
	for _, key := range keysOf(frozenByKey) {
		m := mappings[key]
		switch {
		case m.Status == "done":
			report.Done++
		case pendingTaskPattern.MatchString(m.Status):
			report.Pending++
			report.Gaps = append(report.Gaps, key+" ("+m.Status+")")
		case m.Status == "na":
			report.NA++
		}
		problems := verifyMapping(m, idx)
		for _, p := range problems {
			report.Mapping = append(report.Mapping, Finding{Item: key, Problem: p})
		}
	}

	if mode == ModeFinal && report.Pending > 0 {
		report.Mapping = append(report.Mapping, Finding{
			Item:    "",
			Problem: fmt.Sprintf("final integrity check requires zero pending items, %d remain (registered gaps do not count as coverage)", report.Pending),
		})
	}

	report.Pass = len(report.Added) == 0 && len(report.Removed) == 0 && len(report.Changed) == 0 && len(report.Mapping) == 0
	return report, nil
}

// validateStatus rejects malformed statuses before any comparison runs.
func validateStatus(m Mapping) string {
	switch {
	case m.Status == "done":
		return ""
	case pendingTaskPattern.MatchString(m.Status):
		return ""
	case m.Status == "na":
		if strings.TrimSpace(m.Note) == "" {
			return `status "na" requires a non-empty note stating the reason and the equivalent mapping`
		}
		return ""
	default:
		return fmt.Sprintf("status must be done, na, or pending:WKR-01..12, got %q", m.Status)
	}
}

// verifyMapping checks one entry's five-segment registration. Done entries
// need all four segments present and every reference resolvable; pending and
// na entries verify the references they do declare, so typos fail too.
// Because every done segment must carry resolvable references, a bare
// capability marker (a flag with no worker branch, client method, and
// acceptance test) can never satisfy the final check.
func verifyMapping(m Mapping, idx *RefIndex) []string {
	var problems []string
	segments := []struct {
		name string
		refs string
	}{
		{"dto", m.DTO},
		{"worker", m.Worker},
		{"client", m.Client},
		{"test", m.Test},
	}
	if m.Status == "done" {
		for _, seg := range segments {
			if strings.TrimSpace(seg.refs) == "" {
				problems = append(problems, fmt.Sprintf("done entry has an empty %s segment", seg.name))
			}
		}
	}
	for _, seg := range segments {
		if strings.TrimSpace(seg.refs) == "" {
			continue
		}
		for _, ref := range idx.resolveRefs(seg.name, seg.refs) {
			problems = append(problems, fmt.Sprintf("%s reference %q does not resolve", seg.name, ref))
		}
	}
	return problems
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
