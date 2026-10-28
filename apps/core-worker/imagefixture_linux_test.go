//go:build linux

// This file verifies the frozen image transfer contract end to end: the real
// core-worker binary loads a controlled native library (compiled at test time)
// whose AsstGetImage/AsstGetImageBgr serve deterministic images of configured
// sizes, and the supervision client assembles the chunked transfer and
// compares byte for byte and by digest. The sizes cross the legacy 8 MiB
// single-frame limit and reach the binding default maxima (PNG 32 MiB, BGR
// 64 MiB) without touching any device: the fixture never screenscaps, never
// connects, and never starts a task.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const (
	imageFixtureSrcRel = "testdata/imagefixture/fixture.c"
	imageFixtureSOName = "libimagefixture.so"

	imageFixturePNGSizeEnv = "RHODECK_IMAGE_FIXTURE_PNG_BYTES"
	imageFixtureBGRSizeEnv = "RHODECK_IMAGE_FIXTURE_BGR_BYTES"

	imageFixturePNGSalt byte = 0x50
	imageFixtureBGRSalt byte = 0x47
)

// imageFixturePattern mirrors fixture.c's deterministic fill:
// byte[i] = i*131 + salt (mod 256). The C and Go sides are maintained
// independently; any byte divergence is visible in the comparison.
func imageFixturePattern(size int, salt byte) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(uint64(i)*131 + uint64(salt))
	}
	return out
}

// buildImageFixture compiles the controlled native library and returns the
// .so absolute path. Missing cc/gcc is a test-environment limitation and is
// reported as a skip, never as a pass.
func buildImageFixture(t *testing.T) string {
	t.Helper()
	compiler := ""
	for _, name := range []string{"cc", "gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Skipf("image fixture blocked: no C compiler found; controlled native verification not executed, does not count as a pass")
	}
	src, err := filepath.Abs(imageFixtureSrcRel)
	if err != nil {
		t.Fatalf("resolve fixture source: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("image fixture source %s is not accessible: %v", src, err)
	}
	outPath := filepath.Join(t.TempDir(), imageFixtureSOName)
	args := []string{"-shared", "-fPIC", "-O2", "-o", outPath, src}
	t.Logf("image fixture build: compiler=%s platform=%s/%s command=%s", compiler, runtime.GOOS, runtime.GOARCH, compiler+" "+strings.Join(args, " "))
	output, err := exec.Command(compiler, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to compile the image fixture: %v\n%s", err, output)
	}
	return outPath
}

// launchImageFixtureWorker starts one real worker process against the
// controlled native library with the configured image sizes.
func launchImageFixtureWorker(t *testing.T, library string, pngBytes, bgrBytes uint64) *worker.Client {
	t.Helper()
	c, err := worker.Launch(context.Background(), worker.Config{
		Executable:      workerBinary(t),
		Env:             []string{fmt.Sprintf("%s=%d", imageFixturePNGSizeEnv, pngBytes), fmt.Sprintf("%s=%d", imageFixtureBGRSizeEnv, bgrBytes)},
		StartupTimeout:  15 * time.Second,
		RequestTimeout:  60 * time.Second,
		ShutdownTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// initializeImageFixtureWorker initializes the fixture instance and checks
// the capability snapshot: the optional BGR symbol is exported, so the
// capability must be reported instead of a per-command guess.
func initializeImageFixtureWorker(t *testing.T, c *worker.Client, library string) protocol.RuntimeInfo {
	t.Helper()
	root := t.TempDir()
	info, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "image-fixture", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}})
	if err != nil {
		t.Fatalf("initialize: %v; diagnostics: %s", err, c.Diagnostics())
	}
	if !info.Capabilities.ImageBGR || info.NullSize != math.MaxUint64 {
		t.Fatalf("fixture runtime snapshot: %+v", info)
	}
	return info
}

// TestControlledNativeImageTransferFidelity proves the whole chain on large
// images: worker reads the controlled native cache, serves 4 MiB chunks, the
// client assembles and verifies, and the returned snapshot matches the
// deterministic pattern byte for byte and by SHA256 — including sizes above
// the legacy 8 MiB single-frame limit, the binding default maxima, and legal
// empty images. A repeat read returns an independent, unmodified snapshot.
func TestControlledNativeImageTransferFidelity(t *testing.T) {
	library := buildImageFixture(t)
	cases := []struct {
		name string
		png  uint64
		bgr  uint64
	}{
		{"above the legacy single-frame limit", 9 << 20, 0},
		{"binding default maxima", 32 << 20, 64 << 20},
		{"legal empty images", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := launchImageFixtureWorker(t, library, tc.png, tc.bgr)
			initializeImageFixtureWorker(t, c, library)

			wantPNG := imageFixturePattern(int(tc.png), imageFixturePNGSalt)
			gotPNG, err := c.GetImagePNG(context.Background())
			if err != nil {
				t.Fatalf("get_image_png: %v; diagnostics: %s", err, c.Diagnostics())
			}
			if !bytes.Equal(gotPNG, wantPNG) {
				t.Fatalf("png snapshot diverges byte for byte: got %d bytes, want %d", len(gotPNG), len(wantPNG))
			}
			pngDigest := sha256.Sum256(gotPNG)
			wantDigest := sha256.Sum256(wantPNG)
			if !bytes.Equal(pngDigest[:], wantDigest[:]) {
				t.Fatal("png digest diverges from the controlled native content")
			}

			wantBGR := imageFixturePattern(int(tc.bgr), imageFixtureBGRSalt)
			gotBGR, err := c.GetImageBGR(context.Background())
			if err != nil {
				t.Fatalf("get_image_bgr: %v; diagnostics: %s", err, c.Diagnostics())
			}
			if !bytes.Equal(gotBGR, wantBGR) {
				t.Fatalf("bgr snapshot diverges byte for byte: got %d bytes, want %d", len(gotBGR), len(wantBGR))
			}

			// A repeat read is an independent snapshot of the same cache:
			// mutating the first return value cannot reach it.
			if len(gotPNG) > 0 {
				gotPNG[0] ^= 0xFF
			}
			again, err := c.GetImagePNG(context.Background())
			if err != nil {
				t.Fatalf("repeat get_image_png: %v", err)
			}
			if !bytes.Equal(again, wantPNG) {
				t.Fatalf("repeat png snapshot diverges after caller mutation: %d bytes", len(again))
			}

			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("ping after transfers: %v", err)
			}
			// The fixture never calls back: reads and chunk traffic must not
			// have produced any event.
			select {
			case event := <-c.Events():
				t.Fatalf("fixture transfer produced events: %+v", event)
			default:
			}
			if err := c.Close(context.Background()); err != nil {
				t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
			}
			bgrDigest := sha256.Sum256(wantBGR)
			t.Logf("png=%d bytes sha256=%s bgr=%d bytes sha256=%s", len(gotPNG), hex.EncodeToString(wantDigest[:]), len(gotBGR), hex.EncodeToString(bgrDigest[:]))
		})
	}
}
