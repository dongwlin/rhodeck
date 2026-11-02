package control

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// This is opt-in and never connects, appends or starts a task. A skipped test
// does not establish compatibility with any native installation.
func TestRealCoreDoctorNoDevice(t *testing.T) {
	library := os.Getenv("RHODECK_WORKER_CORE_LIBRARY")
	root := os.Getenv("RHODECK_WORKER_RESOURCE_ROOT")
	if library == "" || root == "" {
		t.Skip("explicit Core library and resource root are required")
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "worker")
	if filepath.Ext(os.Args[0]) == ".exe" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-o", executable, "../../apps/worker")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build worker: %s", output)
	userDir := filepath.Join(dir, "core-user")
	err = os.Mkdir(userDir, 0700)
	require.NoError(t, err)
	dataDir := filepath.Join(dir, "control")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := Doctor(ctx, Config{DataDir: dataDir, Worker: worker.Config{Executable: executable}}, protocol.InitializeRequest{
		SnapshotID: "explicit-no-device-doctor", LibraryPath: library, UserDir: userDir, ResourceRoots: []string{root},
		ExpectedCoreVersion: os.Getenv("RHODECK_WORKER_CORE_VERSION"), ExpectedLibrarySHA256: os.Getenv("RHODECK_WORKER_LIBRARY_SHA256"),
	})
	require.NoError(t, err)
	require.False(t, info.CoreVersion == "" || info.LibrarySHA256 == "" || len(info.Resources) != 1 || info.Limits == nil, "missing installation snapshot")
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	require.NoError(t, err)
	require.Empty(t, entries, "doctor created a business run record")
	s, err := openStore(dataDir)
	require.NoError(t, err, "doctor retained ownership")
	_ = s.close()
}
