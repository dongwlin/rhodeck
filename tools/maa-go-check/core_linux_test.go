package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCoreExpectationsAreNotHiddenByMissingInput(t *testing.T) {
	root, err := resolveRoot("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"MAA_CORE_LIB_PATH", "MAA_CORE_RESOURCE_ROOT", "MAA_CORE_TARBALL", "MAA_CORE_EXPECTED_VERSION", "MAA_CORE_EXPECTED_LIBRARY_SHA256", "MAA_CORE_EXPECTED_TARBALL_SHA256", "MAA_CORE_EXPECTED_VERSION_SHA256"} {
		t.Setenv(name, "")
	}
	tests := []struct {
		name, variable, value, status string
	}{
		{"valid expectation missing input", "MAA_CORE_EXPECTED_LIBRARY_SHA256", strings.Repeat("a", 64), statusUnverified},
		{"malformed expectation missing input", "MAA_CORE_EXPECTED_LIBRARY_SHA256", "invalid", statusFail},
		{"orphan tarball expectation", "MAA_CORE_EXPECTED_TARBALL_SHA256", strings.Repeat("a", 64), statusFail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.variable, tc.value)
			env := environment{Root: root, CoreExpectations: hasCoreExpectations([]string{tc.variable + "=" + tc.value})}
			res := runLayer(context.Background(), env, layer{
				Name: "core", Requires: []string{"core-input"},
				Run: func(ctx context.Context, env environment) layerResult {
					return runGoTest(ctx, env, goTestStep{
						Name: "core input admission", Packages: []string{"./packages/maa-go/"},
						Run: "^TestRealCoreOpenSucceedsInFreshProcess$", Env: noCGO, Timeout: time.Minute,
					})
				},
			})
			if res.Status != tc.status {
				t.Fatalf("core status = %q, want %q (%s)", res.Status, tc.status, res.Detail)
			}
			if tc.status == statusUnverified && res.Skipped == 0 {
				t.Fatalf("valid expectation did not reach the acceptance tests: %+v", res)
			}
		})
	}
}
