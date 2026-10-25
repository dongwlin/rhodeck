// profile_env_test.go 校验锁定 profile 常量、注册表行为、目标校验与
// 冻结宏环境;期望值逐字节转写自公开上游归档清单,防止转录错误。

package main

import (
	"strings"
	"testing"
)

// TestLockedProfileDigests 钉死六个归档文件的用途、大小与 SHA-256。
func TestLockedProfileDigests(t *testing.T) {
	want := map[string]struct {
		purpose string
		size    int64
		sha     string
	}{
		FileAsstCaller:      {PurposeCheckerInput, 5413, "8f1245d55198364bc15beaa2ff91456c747e258beb7a7547f80751563f8c94b7"},
		FileAsstPort:        {PurposeCheckerInput, 1087, "1c99ea5ceb121e323fefbd4116e344efa73640b719474b738aad1b15b6de4a64"},
		FileAsstCallerExtra: {PurposeCheckerInput, 1312, "9229054cf8cd6a36eb4ae59e4927fb3e16de89800bf922f96b67d0c672450737"},
		FileAsstTypes:       {PurposeSemanticReference, 29622, "884f71e8804808aa79a1c01249532c4ada462ea8fdbc83c66b6ed9a751b3200b"},
		FileAsstMsg:         {PurposeSemanticReference, 2636, "f6a8b551effa5a3a8cca42805b5a09c9cfa7f53a3f3e01f4bad5b858ee6df7c6"},
		FileLicense:         {PurposeUpstreamLicense, 34523, "8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef"},
	}
	p := newLockedProfile()
	if p.ID != LockedProfileID || p.SourceTag != "v6.20.1" ||
		p.SourceCommit != "d94dd007aca3b519395fb1f0aee26c7bd5ba5d5f" ||
		p.SourceRepository != "https://github.com/MaaAssistantArknights/MaaAssistantArknights" {
		t.Fatalf("locked profile identity mismatch: %+v", p)
	}
	if len(p.Files) != len(want) {
		t.Fatalf("locked profile has %d files, want %d", len(p.Files), len(want))
	}
	for path, w := range want {
		f, ok := p.Files[path]
		if !ok {
			t.Fatalf("locked profile is missing %s", path)
		}
		if f.Purpose != w.purpose || f.Size != w.size || f.SHA256 != w.sha {
			t.Errorf("%s = %+v, want purpose %s size %d sha %s", path, f, w.purpose, w.size, w.sha)
		}
	}
}

// TestLookupProfileRegistryShape 生产注册表只含锁定 profile;未注册
// ID 一律失败,测试注入的 profile 用后即恢复。
func TestLookupProfileRegistryShape(t *testing.T) {
	if _, ok := LookupProfile("maa-v9.9.9"); ok {
		t.Error("unknown profile id must not resolve")
	}
	if _, ok := LookupProfile(""); ok {
		t.Error("empty profile id must not resolve")
	}
	p, ok := LookupProfile(LockedProfileID)
	if !ok {
		t.Fatal("locked profile must resolve")
	}
	if p.ID != LockedProfileID || len(p.Files) != 6 {
		t.Errorf("unexpected locked profile: %+v", p)
	}

	TP := Profile{ID: "injected", SourceTag: "t", SourceCommit: "c",
		Files: map[string]ProfileFile{"x": {Purpose: PurposeCheckerInput, Size: 1, SHA256: "aa"}}}
	registerTestProfile(t, TP)
	got, ok := LookupProfile("injected")
	if !ok || got.ID != "injected" {
		t.Fatalf("injected test profile must resolve, got %+v ok=%v", got, ok)
	}
}

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
