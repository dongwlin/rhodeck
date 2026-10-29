package control

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	executable := filepath.Join(dir, "core-worker")
	if filepath.Ext(os.Args[0]) == ".exe" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-o", executable, "../../apps/core-worker")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v: %s", err, output)
	}
	userDir := filepath.Join(dir, "core-user")
	if err := os.Mkdir(userDir, 0700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "control")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := Doctor(ctx, Config{DataDir: dataDir, Worker: worker.Config{Executable: executable}}, protocol.InitializeRequest{
		SnapshotID: "explicit-no-device-doctor", LibraryPath: library, UserDir: userDir, ResourceRoots: []string{root},
		ExpectedCoreVersion: os.Getenv("RHODECK_WORKER_CORE_VERSION"), ExpectedLibrarySHA256: os.Getenv("RHODECK_WORKER_LIBRARY_SHA256"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.CoreVersion == "" || info.LibrarySHA256 == "" || len(info.Resources) != 1 || info.Limits == nil {
		t.Fatal("missing installation snapshot")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("doctor created a business run record")
	}
	s, err := openStore(dataDir)
	if err != nil {
		t.Fatalf("doctor retained ownership: %v", err)
	}
	_ = s.close()
}
