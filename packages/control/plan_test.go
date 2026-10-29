package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func validTestPlan(t *testing.T) Plan {
	t.Helper()
	dir := t.TempDir()
	return Plan{SchemaVersion: SchemaVersion, HostRunID: "external-run", Initialize: protocol.InitializeRequest{
		SnapshotID: "test-install", LibraryPath: filepath.Join(dir, "MaaCore"), UserDir: dir, ResourceRoots: []string{dir},
	}, Connection: protocol.ConnectRequest{ADBPath: filepath.Join(dir, "adb"), Address: "fixture"}, Tasks: []Task{{ID: "task-1", Type: "FutureTask", Params: json.RawMessage(`{"large":9007199254740993,"unknown":{"a":1}}`)}}}
}

func TestPlanOpenJSONAndSnapshot(t *testing.T) {
	p := validTestPlan(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePlan(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got.Tasks[0].Params, []byte("9007199254740993")) || got.Tasks[0].Type != "FutureTask" {
		t.Fatal("open task input changed")
	}
	copy := clonePlan(got)
	got.Tasks[0].Params[2] = 'X'
	got.Initialize.ResourceRoots[0] = "changed"
	if !bytes.Equal(copy.Tasks[0].Params, p.Tasks[0].Params) || copy.Initialize.ResourceRoots[0] != p.Initialize.ResourceRoots[0] {
		t.Fatal("snapshot aliases caller input")
	}
}

func TestPlanRejectsAmbiguousControls(t *testing.T) {
	p := validTestPlan(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]string{
		"top duplicate":    strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"nested duplicate": strings.Replace(string(raw), `"snapshot_id":"test-install"`, `"snapshot_id":"test-install","snapshot_id":"other"`, 1),
		"task duplicate":   strings.Replace(string(raw), `"type":"FutureTask"`, `"type":"FutureTask","type":"Other"`, 1),
		"unknown control":  strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"typo":true`, 1),
		"trailing":         string(raw) + ` {}`,
		"null":             "null",
		"invalid utf8":     string(raw) + string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePlan(strings.NewReader(input)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// Duplicate native fields remain an open Core concern, never decoded into
	// floating-point values by this application.
	p.Tasks[0].Params = json.RawMessage(`{"future":1,"future":9007199254740993}`)
	raw, _ = json.Marshal(p)
	if _, err := DecodePlan(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestPlanAdmissionRejections(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"version":          func(p *Plan) { p.SchemaVersion = 2 },
		"empty tasks":      func(p *Plan) { p.Tasks = nil },
		"too many tasks":   func(p *Plan) { p.Tasks = make([]Task, MaxTasks+1) },
		"duplicate ID":     func(p *Plan) { p.Tasks = append(p.Tasks, p.Tasks[0]) },
		"params array":     func(p *Plan) { p.Tasks[0].Params = json.RawMessage(`[]`) },
		"params null":      func(p *Plan) { p.Tasks[0].Params = json.RawMessage(`null`) },
		"large params":     func(p *Plan) { p.Tasks[0].Params = json.RawMessage(`{"x":"` + strings.Repeat("x", 1<<20) + `"}`) },
		"relative library": func(p *Plan) { p.Initialize.LibraryPath = "relative" },
		"relative adb":     func(p *Plan) { p.Connection.ADBPath = "relative" },
		"nul type":         func(p *Plan) { p.Tasks[0].Type = "Future\x00" },
		"large snapshot":   func(p *Plan) { p.Initialize.SnapshotID = strings.Repeat("x", 257) },
		"invalid sha":      func(p *Plan) { p.Initialize.ExpectedLibrarySHA256 = strings.Repeat("z", 64) },
		"duplicate option": func(p *Plan) { p.Initialize.StaticOptions = []protocol.Option{{Key: 1}, {Key: 1}} },
	} {
		t.Run(name, func(t *testing.T) {
			p := validTestPlan(t)
			mutate(&p)
			if err := ValidatePlan(p); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if _, err := DecodePlan(ioTooLarge{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("oversize input: %v", err)
	}
}

type ioTooLarge struct{}

func (ioTooLarge) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}
