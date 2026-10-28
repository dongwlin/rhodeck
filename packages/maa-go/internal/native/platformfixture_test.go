//go:build linux || darwin || windows

package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebitengine/purego"
)

const (
	platformLoaderChildEnv = "MAA_PLATFORM_LOADER_CHILD"
	platformLoaderPathEnv  = "MAA_PLATFORM_LOADER_PATH"
	platformLoaderOK       = "MAA_PLATFORM_LOADER_OK"
)

// Each load runs in a fresh test process: libraries are intentionally never
// unloaded, and a previous successful load must not conceal a missing dependency.
func TestPlatformLoaderCalls(t *testing.T) {
	if os.Getenv(platformLoaderChildEnv) == "" {
		path, _ := buildPlatformLoaderFixture(t)
		runPlatformLoaderChild(t, t.Name(), path)
		return
	}
	path := os.Getenv(platformLoaderPathEnv)
	lib, err := NewLoader().Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	if lib.Path() != path || lib.Name() != filepath.Base(path) {
		t.Fatalf("loaded library path/name = %q/%q", lib.Path(), lib.Name())
	}

	addr, err := lib.Lookup("platform_roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip func(uint64) uint64
	purego.RegisterFunc(&roundtrip, addr)
	// The result includes a value obtained from the separately loaded dependency.
	if got := roundtrip(0x12345678abcdef01); got != 0xece8ece0dd99dd11 {
		t.Fatalf("native uint64 roundtrip = %#x, want %#x", got, uint64(0xece8ece0dd99dd11))
	}

	const absent = "platform_symbol_that_does_not_exist"
	addr, err = lib.Lookup(absent)
	var lookupErr *SymbolLookupError
	if addr != 0 || !errors.As(err, &lookupErr) {
		t.Fatalf("Lookup(%q) = %#x, %v; want structured lookup failure", absent, addr, err)
	}
	if lookupErr.LibraryPath != path || lookupErr.Symbol != absent || lookupErr.Profile != abiProfile || lookupErr.Err == nil {
		t.Fatalf("lookup diagnostics = %+v", lookupErr)
	}

	addr, err = lib.Lookup("platform_emit_callback")
	if err != nil {
		t.Fatal(err)
	}
	var emitCallback func(uintptr, uintptr) int32
	purego.RegisterFunc(&emitCallback, addr)
	type event struct {
		msg     int32
		details string
		valid   bool
	}
	events := make(chan event, 1)
	var calls atomic.Int32
	token, err := RegisterCallbackTarget(func(msg int32, details *byte) {
		length, valid := CallbackDetailsLength(details, 128)
		var copied string
		if valid {
			copied = string(CopyCallbackDetails(details, length))
		}
		calls.Add(1)
		select {
		case events <- event{msg: msg, details: copied, valid: valid}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer UnregisterCallbackTarget(token)
	if got := emitCallback(CallbackTrampoline(), token); got != 7 {
		t.Fatalf("native callback function = %d, want 7", got)
	}
	select {
	case got := <-events:
		const wantDetails = `{"source":"platform-loader","value":9007199254740993}`
		if !got.valid || got.msg != -12345 || got.details != wantDetails || calls.Load() != 1 {
			t.Fatalf("native callback = %+v, calls=%d", got, calls.Load())
		}
	default:
		t.Fatal("native callback did not reach its registered target")
	}
	fmt.Println(platformLoaderOK)
}

func TestPlatformLoaderMissingDependency(t *testing.T) {
	if os.Getenv(platformLoaderChildEnv) == "" {
		path, dependency := buildPlatformLoaderFixture(t)
		if err := os.Remove(dependency); err != nil {
			t.Fatal(err)
		}
		runPlatformLoaderChild(t, t.Name(), path)
		return
	}
	path := os.Getenv(platformLoaderPathEnv)
	lib, err := NewLoader().Open(path)
	var loadErr *LibraryLoadError
	if lib != nil || !errors.As(err, &loadErr) {
		t.Fatalf("Open(%q) = %v, %v; want dependency load failure", path, lib, err)
	}
	if loadErr.Path != path || loadErr.GOOS != runtime.GOOS || loadErr.GOARCH != runtime.GOARCH || loadErr.Err == nil {
		t.Fatalf("load diagnostics = %+v", loadErr)
	}
	if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("dependency failure did not reach the system loader: %v", err)
	}
	fmt.Println(platformLoaderOK)
}

func runPlatformLoaderChild(t *testing.T, name, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), platformLoaderChildEnv+"="+name, platformLoaderPathEnv+"="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native loader child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), platformLoaderOK) {
		t.Fatalf("native loader child did not confirm assertions:\n%s", out)
	}
	t.Logf("%s", out)
}

// Both libraries live in a directory containing spaces and non-ASCII characters.
// Relative output names keep compiler arguments independent of Windows' current
// code page while the production loader still receives the complete Unicode path.
func buildPlatformLoaderFixture(t *testing.T) (library, dependency string) {
	t.Helper()
	compiler := ""
	for _, name := range []string{"cc", "gcc", "clang"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Fatal("platform loader fixture requires a native C compiler (cc, gcc, or clang)")
	}
	sourceDir, err := filepath.Abs("testdata/platformfixture")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "原生 loader fixtures")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var mainName, dependencyName string
	var dependencyArgs, mainArgs []string
	dependencySource := filepath.Join(sourceDir, "dependency.c")
	mainSource := filepath.Join(sourceDir, "fixture.c")
	switch runtime.GOOS {
	case "linux":
		mainName, dependencyName = "libplatformfixture.so", "libplatformfixture_dep.so"
		dependencyArgs = []string{"-shared", "-fPIC", "-Wl,-soname," + dependencyName, "-o", dependencyName, dependencySource}
		mainArgs = []string{"-shared", "-fPIC", "-o", mainName, mainSource, "-L.", "-lplatformfixture_dep", "-Wl,-rpath,$ORIGIN", "-Wl,-z,defs"}
	case "darwin":
		mainName, dependencyName = "libplatformfixture.dylib", "libplatformfixture_dep.dylib"
		dependencyArgs = []string{"-dynamiclib", "-fPIC", "-Wl,-install_name,@rpath/" + dependencyName, "-o", dependencyName, dependencySource}
		mainArgs = []string{"-dynamiclib", "-fPIC", "-o", mainName, mainSource, "-L.", "-lplatformfixture_dep", "-Wl,-rpath,@loader_path"}
	case "windows":
		mainName, dependencyName = "platformfixture.dll", "platformfixture_dep.dll"
		dependencyArgs = []string{"-shared", "-o", dependencyName, dependencySource, "-Wl,--out-implib,libplatformfixture_dep.dll.a"}
		mainArgs = []string{"-shared", "-o", mainName, mainSource, "-L.", "-lplatformfixture_dep"}
	}
	for _, args := range [][]string{dependencyArgs, mainArgs} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		cmd := exec.CommandContext(ctx, compiler, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("compile %s/%s fixture: %s %q: %v\n%s", runtime.GOOS, runtime.GOARCH, compiler, args, err, out)
		}
	}
	return filepath.Join(dir, mainName), filepath.Join(dir, dependencyName)
}
