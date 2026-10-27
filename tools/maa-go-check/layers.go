// Layer definitions for maa-go-check. Each layer is independently selected
// and reported; the "ci" alias only contains layers that are runnable on a
// clean Linux amd64 machine with a C compiler and the vendored checker
// input. The real-MaaCore layer is local-only.
package main

import (
	"context"
	"time"
)

type layer struct {
	Name        string
	Description string
	Requires    []string
	Run         func(context.Context, environment) layerResult
}

func definitions() []layer {
	return []layer{
		{
			Name:        "static",
			Description: "locked C/Go declaration checker (linux+windows)",
			Run:         runStatic,
		},
		{
			Name:        "go",
			Description: "Go unit and fault-injection tests (no native)",
			Run:         runGo,
		},
		{
			Name:        "loader",
			Description: "loader fixture in a fresh subprocess",
			Requires:    []string{"cc"},
			Run:         runLoader,
		},
		{
			Name:        "abi",
			Description: "real C ABI fixture calls",
			Requires:    []string{"cc"},
			Run:         runABI,
		},
		{
			Name:        "callback",
			Description: "native-thread callback fixture",
			Requires:    []string{"cc"},
			Run:         runCallback,
		},
		{
			Name:        "core",
			Description: "real MaaCore no-device chain",
			Requires:    []string{"tarball"},
			Run:         runCore,
		},
		{
			Name:        "build",
			Description: "CGO_ENABLED=0 build and vet",
			Run:         runBuild,
		},
		{
			Name:        "fmt",
			Description: "gofmt check",
			Run:         runFmt,
		},
		{
			Name:        "race",
			Description: "Go race detector (CGO_ENABLED=1, separately recorded)",
			Requires:    []string{"cc"},
			Run:         runRace,
		},
	}
}

// runStatic combines the checker's own unit/counterexample suite with two
// real runs over the repository-internal locked C input. The tarball-backed
// real-header tests are excluded here because the vendored input is the CI
// source; runCheckerTarget asserts the policy counts independently.
func runStatic(ctx context.Context, env environment) layerResult {
	res := newLayerResult("static")
	res.Detail = "checker tests plus locked C input (linux/windows)"
	res.merge(runGoTest(ctx, env, goTestStep{
		Name:     "checker tests",
		Packages: []string{"./tools/check-maa-api/"},
		Skip:     "TestRealHeaderCheckerEndToEnd|TestRealHeaderSignatureSection",
		Env:      noCGO,
		Timeout:  10 * time.Minute,
	}))
	if res.Status == statusFail {
		return res
	}
	bin, cleanup, err := buildChecker(ctx, env)
	if err != nil {
		res.Status = statusFail
		res.Failed++
		res.Failures = append(res.Failures, err.Error())
		return res
	}
	defer cleanup()
	res.merge(runCheckerTarget(ctx, env, bin, "linux", 25, 1, 4))
	res.merge(runCheckerTarget(ctx, env, bin, "windows", 26, 0, 4))
	return res
}

// runGo runs the fake-native, lifecycle, snapshot, and tool unit tests. The
// real-library acceptance tests are selected out; they have their own layer
// so a missing tarball cannot hide here.
func runGo(ctx context.Context, env environment) layerResult {
	res := newLayerResult("go")
	res.merge(runGoTest(ctx, env, goTestStep{
		Name:     "maa-go unit tests",
		Packages: []string{"./packages/maa-go/"},
		Skip:     "TestRealCore",
		Env:      noCGO,
		Timeout:  10 * time.Minute,
	}))
	res.merge(runGoTest(ctx, env, goTestStep{
		Name:     "ffitest executor",
		Packages: []string{"./packages/maa-go/internal/ffitest/"},
		Env:      noCGO,
		Timeout:  5 * time.Minute,
	}))
	res.merge(runGoTest(ctx, env, goTestStep{
		Name:     "native unit tests",
		Packages: []string{"./packages/maa-go/internal/native/"},
		Skip:     "TestLoaderFixture|TestABIFixture|TestCallbackFixture",
		Env:      noCGO,
		Timeout:  10 * time.Minute,
	}))
	res.merge(runGoTest(ctx, env, goTestStep{
		Name:     "checker tool tests",
		Packages: []string{"./tools/maa-go-check/"},
		Env:      noCGO,
		Timeout:  5 * time.Minute,
	}))
	if res.Status == statusPass {
		res.Detail = "unit and fault-injection tests passed"
	}
	return res
}

func runLoader(ctx context.Context, env environment) layerResult {
	res := runGoTest(ctx, env, goTestStep{
		Name:     "loader fixture",
		Packages: []string{"./packages/maa-go/internal/native/"},
		Run:      "TestLoaderFixture",
		Env:      noCGO,
		Timeout:  5 * time.Minute,
	})
	if res.Status == statusPass {
		res.Detail = "loader fixture passed; empty exports prove loading only and are never called"
	}
	return res
}

func runABI(ctx context.Context, env environment) layerResult {
	res := runGoTest(ctx, env, goTestStep{
		Name:     "abi fixture",
		Packages: []string{"./packages/maa-go/internal/native/"},
		Run:      "TestABIFixture",
		Env:      noCGO,
		Timeout:  5 * time.Minute,
	})
	if res.Status == statusPass {
		res.Detail = "real C ABI fixture passed (numbers, params, void, buffers)"
	}
	return res
}

func runCallback(ctx context.Context, env environment) layerResult {
	res := runGoTest(ctx, env, goTestStep{
		Name:     "callback fixture",
		Packages: []string{"./packages/maa-go/internal/native/"},
		Run:      "TestCallbackFixture",
		Env:      noCGO,
		Timeout:  5 * time.Minute,
	})
	if res.Status == statusPass {
		res.Detail = "native-thread callback fixture passed"
	}
	return res
}

func runCore(ctx context.Context, env environment) layerResult {
	res := runGoTest(ctx, env, goTestStep{
		Name:     "real core",
		Packages: []string{"./packages/maa-go/"},
		Run:      "TestRealCore",
		Env:      noCGO,
		Timeout:  30 * time.Minute,
	})
	if res.Status == statusPass {
		res.Detail = "real MaaCore v6.20.1 no-device chain passed"
	}
	return res
}

func runBuild(ctx context.Context, env environment) layerResult {
	res := newLayerResult("build")
	res.merge(runCommand(ctx, env, "go build ./...", []string{"go", "build", "./..."}, noCGO, 5*time.Minute, false))
	res.merge(runCommand(ctx, env, "go vet ./...", []string{"go", "vet", "./..."}, noCGO, 5*time.Minute, false))
	if res.Status == statusPass {
		res.Detail = "CGO_ENABLED=0 build and vet passed"
	}
	return res
}

func runFmt(ctx context.Context, env environment) layerResult {
	res := runCommand(ctx, env, "gofmt -l .", []string{"gofmt", "-l", "."}, nil, 2*time.Minute, true)
	if res.Status == statusPass {
		res.Detail = "gofmt found no unformatted files"
	}
	return res
}

// raceGoTestStep is the single source of the race layer's test spec. It must
// carry Race: true; without the actual -race flag the layer would only run
// the ordinary test binary and report a false green.
func raceGoTestStep() goTestStep {
	return goTestStep{
		Name:     "race detector",
		Packages: []string{"./packages/maa-go/..."},
		Skip:     "TestRealCore",
		Race:     true,
		Env:      []string{"CGO_ENABLED=1", "GOWORK=off"},
		Timeout:  25 * time.Minute,
	}
}

func runRace(ctx context.Context, env environment) layerResult {
	res := runGoTest(ctx, env, raceGoTestStep())
	if res.Status == statusPass {
		res.Detail = "race passed (-race, CGO_ENABLED=1); -skip TestRealCore (real-core acceptance in the core layer)"
	}
	return res
}
