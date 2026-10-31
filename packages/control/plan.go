package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// DecodePlan bounds allocation and rejects ambiguous control fields. Native
// params and connection-extra JSON remain open objects with original numbers.
func DecodePlan(r io.Reader) (Plan, error) {
	var p Plan
	if r == nil {
		return p, fmt.Errorf("%w: missing input", ErrInvalidPlan)
	}
	raw, err := io.ReadAll(io.LimitReader(r, MaxPlanBytes+1))
	if err != nil {
		return p, fmt.Errorf("%w: cannot read input", ErrInvalidPlan)
	}
	if len(raw) > MaxPlanBytes {
		return p, fmt.Errorf("%w: input exceeds byte limit", ErrInvalidPlan)
	}
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := checkControlKeys(keys); err != nil {
		return p, fmt.Errorf("%w: ambiguous control object", ErrInvalidPlan)
	}
	if err := protocol.Decode(raw, &p); err != nil {
		return Plan{}, fmt.Errorf("%w: invalid schema", ErrInvalidPlan)
	}
	if err := ValidatePlan(p); err != nil {
		return Plan{}, err
	}
	return p, nil
}

func checkControlKeys(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := t.(string)
			if !ok || seen[key] {
				return ErrInvalidPlan
			}
			seen[key] = true
			if key == "params" || key == "json" {
				err = d.Decode(new(json.RawMessage))
			} else {
				err = checkControlKeys(d)
			}
			if err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkControlKeys(d); err != nil {
				return err
			}
		}
	default:
		return ErrInvalidPlan
	}
	_, err = d.Token()
	return err
}

// ValidatePlan performs local syntax, input and wire-budget checks only. Core
// task semantics, complete binding-limit validation and device readiness require
// the selected worker and installation; dry-run does not load that installation.
func ValidatePlan(p Plan) error {
	bad := func(s string) error { return fmt.Errorf("%w: %s", ErrInvalidPlan, s) }
	if p.SchemaVersion != SchemaVersion {
		return bad("unsupported schema version")
	}
	if !validText(p.HostRunID, 256, true) {
		return bad("invalid host run ID")
	}
	if err := validateInitialize(p.Initialize); err != nil {
		return err
	}
	strMax, jsonMax, configMax, _ := inputBudgets(p.Initialize)
	c := p.Connection
	if !validPath(c.ADBPath, strMax) || !validText(c.Address, strMax, false) || !validText(c.Config, strMax, true) {
		return bad("invalid connection")
	}
	if uint64(len(c.ADBPath))+uint64(len(c.Address))+uint64(len(c.Config)) > configMax {
		return bad("connection exceeds text budget")
	}
	if err := checkFrame(protocol.Connect, c); err != nil {
		return err
	}
	if len(p.Tasks) == 0 || len(p.Tasks) > MaxTasks {
		return bad("invalid task count")
	}
	seen := make(map[string]bool, len(p.Tasks))
	for _, t := range p.Tasks {
		if !validText(t.ID, 128, false) || seen[t.ID] {
			return bad("invalid or duplicate task ID")
		}
		seen[t.ID] = true
		if t.Adapter != nil {
			if !validAdapterRef(*t.Adapter) {
				return bad("invalid adapter")
			}
			if len(t.Params) > MaxAdapterParamsBytes {
				return bad("adapter params exceed byte limit")
			}
		}
		if !validText(t.Type, strMax, false) || !validObject(t.Params, jsonMax) {
			return bad("invalid task input")
		}
		if uint64(len(t.Type))+uint64(len(t.Params)) > configMax {
			return bad("task exceeds text budget")
		}
		if err := checkFrame(protocol.AppendTask, protocol.TaskRequest{Type: t.Type, Params: t.Params}); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > MaxPlanBytes {
		return bad("plan exceeds byte limit")
	}
	return nil
}

func inputBudgets(i protocol.InitializeRequest) (str, object, config, entries uint64) {
	// Input defaults of the frozen worker v1 binding profile. This is a local
	// admission bound, not a duplicate of native limit cross-field validation.
	if i.Limits == nil {
		return 65536, 1 << 20, protocol.MaxConfigBytes, 256
	}
	l := i.Limits
	return l.InputStringBytes, l.InputJSONBytes, l.ConfigBytes, l.ConfigEntries
}

func validateInitialize(i protocol.InitializeRequest) error {
	bad := func(s string) error { return fmt.Errorf("%w: %s", ErrInvalidPlan, s) }
	if i.Limits != nil {
		if err := protocol.CheckWireLimits(i.Limits); err != nil {
			return bad("limits exceed wire budget")
		}
	}
	strMax, jsonMax, configMax, entries := inputBudgets(i)
	if strMax == 0 || jsonMax == 0 || configMax == 0 || entries == 0 {
		return bad("zero input budget")
	}
	if !validText(i.SnapshotID, 256, false) || !validPath(i.LibraryPath, strMax) || !validPath(i.UserDir, strMax) || len(i.ResourceRoots) == 0 {
		return bad("invalid installation paths or snapshot ID")
	}
	if !validText(i.ExpectedCoreVersion, strMax, true) {
		return bad("invalid expected Core version")
	}
	if i.ExpectedLibrarySHA256 != "" {
		if len(i.ExpectedLibrarySHA256) != 64 {
			return bad("invalid library digest")
		}
		for _, c := range i.ExpectedLibrarySHA256 {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return bad("invalid library digest")
			}
		}
	}
	var total uint64
	add := func(s string) bool {
		if !validText(s, strMax, true) {
			return false
		}
		total += uint64(len(s))
		return total <= configMax
	}
	for _, s := range []string{i.SnapshotID, i.LibraryPath, i.UserDir, i.ExpectedCoreVersion, i.ExpectedLibrarySHA256} {
		if !add(s) {
			return bad("installation exceeds text budget")
		}
	}
	for _, root := range i.ResourceRoots {
		if !validPath(root, strMax) || !add(root) {
			return bad("invalid resource root or budget")
		}
	}
	for _, opts := range [][]protocol.Option{i.StaticOptions, i.InstanceOptions} {
		seen := make(map[int32]bool, len(opts))
		for _, opt := range opts {
			if opt.Key == 0 || seen[opt.Key] || !add(opt.Value) {
				return bad("invalid option or budget")
			}
			seen[opt.Key] = true
		}
	}
	seenExtras := make(map[string]bool, len(i.ConnectionExtras))
	for _, extra := range i.ConnectionExtras {
		if !validText(extra.Name, strMax, false) || seenExtras[extra.Name] || !validObject(extra.JSON, jsonMax) || !add(extra.Name) {
			return bad("invalid connection extra")
		}
		seenExtras[extra.Name] = true
		total += uint64(len(extra.JSON))
		if total > configMax {
			return bad("connection extras exceed text budget")
		}
	}
	if uint64(len(i.ResourceRoots))+uint64(len(i.StaticOptions))+uint64(len(i.InstanceOptions))+uint64(len(i.ConnectionExtras)) > entries {
		return bad("installation exceeds entry budget")
	}
	return checkFrame(protocol.Initialize, i)
}

func validText(s string, max uint64, empty bool) bool {
	return (empty || len(s) > 0) && uint64(len(s)) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func validPath(s string, max uint64) bool { return validText(s, max, false) && filepath.IsAbs(s) }
func validObject(raw json.RawMessage, max uint64) bool {
	b := bytes.TrimSpace(raw)
	return uint64(len(raw)) <= max && len(b) > 0 && b[0] == '{' && utf8.Valid(raw) && json.Valid(raw)
}
func checkFrame(name string, payload any) error {
	m, err := protocol.NewMessage(protocol.Request, name, 1, payload)
	if err == nil {
		_, err = protocol.Encode(m)
	}
	if err != nil {
		return fmt.Errorf("%w: request exceeds wire budget", ErrInvalidPlan)
	}
	return nil
}

func clonePlan(p Plan) Plan {
	p.Initialize.ResourceRoots = append([]string(nil), p.Initialize.ResourceRoots...)
	p.Initialize.StaticOptions = append([]protocol.Option(nil), p.Initialize.StaticOptions...)
	p.Initialize.InstanceOptions = append([]protocol.Option(nil), p.Initialize.InstanceOptions...)
	p.Initialize.ConnectionExtras = append([]protocol.ConnectionExtra(nil), p.Initialize.ConnectionExtras...)
	for j := range p.Initialize.ConnectionExtras {
		p.Initialize.ConnectionExtras[j].JSON = bytes.Clone(p.Initialize.ConnectionExtras[j].JSON)
	}
	if p.Initialize.Limits != nil {
		l := *p.Initialize.Limits
		p.Initialize.Limits = &l
	}
	p.Tasks = append([]Task(nil), p.Tasks...)
	for j := range p.Tasks {
		p.Tasks[j].Params = bytes.Clone(p.Tasks[j].Params)
		if p.Tasks[j].Adapter != nil {
			ref := *p.Tasks[j].Adapter
			p.Tasks[j].Adapter = &ref
		}
	}
	return p
}
