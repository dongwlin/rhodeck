// 本文件校验静态目标与确定性宏环境。

package main

import (
	"strings"
	"testing"
)

func TestValidateTarget(t *testing.T) {
	for _, good := range []Target{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "windows", GOARCH: "amd64"}} {
		if diags := ValidateTarget(good); len(diags) != 0 {
			t.Errorf("target %+v must be accepted, got %v", good, diags)
		}
	}
	for _, bad := range []Target{
		{GOOS: "linux", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"},
		{GOOS: "windows", GOARCH: "386"},
		{GOOS: "", GOARCH: ""},
	} {
		diags := ValidateTarget(bad)
		if len(diags) != 1 || diags[0].Category != CatInput {
			t.Errorf("target %+v must fail with one input diagnostic, got %v", bad, diags)
		}
	}
}

func TestNewMacroEnv(t *testing.T) {
	linux := newMacroEnv(Target{GOOS: "linux", GOARCH: "amd64"}, 0)
	if linux.isDefined("_WIN32") {
		t.Error("_WIN32 must be undefined on linux")
	}
	if !linux.isDefined("__x86_64__") {
		t.Error("__x86_64__ must be defined on both targets")
	}
	if linux.intValue("__GNUC__") != 4 {
		t.Errorf("__GNUC__ on linux = %d, want 4", linux.intValue("__GNUC__"))
	}
	for _, undef := range []string{"__WINE__", "__CYGWIN__", "ASST_CALL", "ASST_DLL_EXPORTS", "__cplusplus"} {
		if linux.isDefined(undef) {
			t.Errorf("%s must be undefined before AsstPort.h", undef)
		}
	}
	if linux.intValue("ASST_WITH_EXTRA_CALLERS") != 0 {
		t.Errorf("ASST_WITH_EXTRA_CALLERS = %d, want 0", linux.intValue("ASST_WITH_EXTRA_CALLERS"))
	}

	windows := newMacroEnv(Target{GOOS: "windows", GOARCH: "amd64"}, 1)
	if !windows.isDefined("_WIN32") {
		t.Error("_WIN32 must be defined on windows")
	}
	if !windows.isDefined("__x86_64__") {
		t.Error("__x86_64__ must be defined on windows")
	}
	if windows.intValue("__GNUC__") != 0 {
		t.Errorf("__GNUC__ on windows = %d, want 0", windows.intValue("__GNUC__"))
	}
	for _, undef := range []string{"__WINE__", "__CYGWIN__", "ASST_CALL", "ASST_DLL_EXPORTS", "__cplusplus"} {
		if windows.isDefined(undef) {
			t.Errorf("%s must be undefined on windows", undef)
		}
	}
	if windows.intValue("ASST_WITH_EXTRA_CALLERS") != 1 {
		t.Errorf("ASST_WITH_EXTRA_CALLERS = %d, want 1", windows.intValue("ASST_WITH_EXTRA_CALLERS"))
	}
}

func TestDiagnosticString(t *testing.T) {
	d := Diagnostic{Category: CatCSyntax, File: "upstream/AsstCaller.h", Line: 75,
		Symbol: "AsstConnect", Expected: "one form", Actual: "another", Message: "broken"}
	s := d.String()
	for _, want := range []string{"c-syntax", "upstream/AsstCaller.h:75", "AsstConnect",
		"expected one form, actual another", "broken"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, want it to contain %q", s, want)
		}
	}
	if got := (Diagnostic{Category: CatInput}).String(); !strings.HasPrefix(got, "check-maa-api: input") {
		t.Errorf("minimal diagnostic = %q", got)
	}
}
