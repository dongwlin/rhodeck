// 本文件校验静态目标与确定性宏环境。

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateTarget(t *testing.T) {
	for _, good := range []Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "windows", GOARCH: "amd64"}} {
		diags := ValidateTarget(good)
		assert.Empty(t, diags, "target %+v must be accepted, got %v", good, diags)
	}
	for _, bad := range []Target{
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "windows", GOARCH: "386"},
		{GOOS: "", GOARCH: ""},
	} {
		diags := ValidateTarget(bad)
		assert.True(t, len(diags) == 1 && diags[0].Category == CatInput,
			"target %+v must fail with one input diagnostic, got %v", bad, diags)
	}
}

func TestNewMacroEnv(t *testing.T) {
	linux := newMacroEnv(Target{GOOS: "linux", GOARCH: "amd64"}, 0)
	assert.False(t, linux.isDefined("_WIN32"), "_WIN32 must be undefined on linux")
	assert.True(t, linux.isDefined("__x86_64__"), "__x86_64__ must be defined on both targets")
	assert.Equal(t, int64(4), linux.intValue("__GNUC__"),
		"__GNUC__ on linux = %d, want 4", linux.intValue("__GNUC__"))
	for _, undef := range []string{"__WINE__", "__CYGWIN__", "ASST_CALL", "ASST_DLL_EXPORTS", "__cplusplus"} {
		assert.False(t, linux.isDefined(undef), "%s must be undefined before AsstPort.h", undef)
	}
	assert.Equal(t, int64(0), linux.intValue("ASST_WITH_EXTRA_CALLERS"),
		"ASST_WITH_EXTRA_CALLERS = %d, want 0", linux.intValue("ASST_WITH_EXTRA_CALLERS"))

	windows := newMacroEnv(Target{GOOS: "windows", GOARCH: "amd64"}, 1)
	assert.True(t, windows.isDefined("_WIN32"), "_WIN32 must be defined on windows")
	assert.True(t, windows.isDefined("__x86_64__"), "__x86_64__ must be defined on windows")
	assert.Equal(t, int64(0), windows.intValue("__GNUC__"),
		"__GNUC__ on windows = %d, want 0", windows.intValue("__GNUC__"))
	for _, undef := range []string{"__WINE__", "__CYGWIN__", "ASST_CALL", "ASST_DLL_EXPORTS", "__cplusplus"} {
		assert.False(t, windows.isDefined(undef), "%s must be undefined on windows", undef)
	}
	assert.Equal(t, int64(1), windows.intValue("ASST_WITH_EXTRA_CALLERS"),
		"ASST_WITH_EXTRA_CALLERS = %d, want 1", windows.intValue("ASST_WITH_EXTRA_CALLERS"))
}

func TestDiagnosticString(t *testing.T) {
	d := Diagnostic{Category: CatCSyntax, File: "upstream/AsstCaller.h", Line: 75,
		Symbol: "AsstConnect", Expected: "one form", Actual: "another", Message: "broken"}
	s := d.String()
	for _, want := range []string{"c-syntax", "upstream/AsstCaller.h:75", "AsstConnect",
		"expected one form, actual another", "broken"} {
		assert.Contains(t, s, want, "String() = %q, want it to contain %q", s, want)
	}
	got := (Diagnostic{Category: CatInput}).String()
	assert.True(t, strings.HasPrefix(got, "check-maa-api: input"), "minimal diagnostic = %q", got)
}
