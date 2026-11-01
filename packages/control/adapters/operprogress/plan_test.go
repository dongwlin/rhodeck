package operprogress

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func validTestPlan(t *testing.T) control.Plan {
	t.Helper()
	dir := t.TempDir()
	return control.Plan{SchemaVersion: control.SchemaVersion, HostRunID: "external-run", Initialize: protocol.InitializeRequest{
		SnapshotID: "test-install", LibraryPath: filepath.Join(dir, "MaaCore"), UserDir: dir, ResourceRoots: []string{dir},
	}, Connection: protocol.ConnectRequest{ADBPath: filepath.Join(dir, "adb"), Address: "fixture"}, Tasks: []control.Task{{ID: "task-1", Type: "FutureTask", Params: json.RawMessage(`{"large":9007199254740993,"unknown":{"a":1}}`)}}}
}
