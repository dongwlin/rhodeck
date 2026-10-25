// cinput_test.go 用合成 fixture 目录校验 c 输入装载:正例通过;缺
// 文件、空文件、摘要/标识不符、绝对/越界/重复路径、未知条目等全部
// 以 input 类诊断失败。测试 profile 由测试自行计算摘要并注册。

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const testProfileID = "test-fixture-v1"

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// positiveFiles 返回与锁定形状一致的正例合成头文件;键与生产 profile
// 的归档路径一致,使解析器的逐文件冻结规则(include 集合、宏表)同
// 样生效。
func positiveFiles(t *testing.T) map[string]string {
	return map[string]string{
		FileAsstCaller:      loadFixture(t, "caller_positive.h"),
		FileAsstPort:        loadFixture(t, "port_positive.h"),
		FileAsstCallerExtra: loadFixture(t, "extra_positive.h"),
	}
}

func fixturePurpose(path string) string {
	switch path {
	case FileAsstCaller, FileAsstPort, FileAsstCallerExtra:
		return PurposeCheckerInput
	default:
		return PurposeSemanticReference
	}
}

func intPtr(i int) *int { return &i }

func sortManifestFiles(fs []manifestFile) {
	sort.Slice(fs, func(i, j int) bool { return fs[i].ArchivePath < fs[j].ArchivePath })
}

// defaultManifest 按文件内容构造与测试 profile 一致的清单。
func defaultManifest(files map[string]string) manifest {
	m := manifest{
		SchemaVersion: intPtr(1),
		ProfileID:     testProfileID,
		Source: manifestSource{
			Repository: "https://example.com/fixture",
			Tag:        "fixture-tag",
			Commit:     strings.Repeat("f", 40),
		},
	}
	for path, content := range files {
		sum := sha256.Sum256([]byte(content))
		m.Files = append(m.Files, manifestFile{
			ArchivePath: path,
			SourcePath:  "fixture/" + path,
			SizeBytes:   int64(len(content)),
			SHA256:      hex.EncodeToString(sum[:]),
			Purpose:     fixturePurpose(path),
		})
	}
	sortManifestFiles(m.Files)
	return m
}

// testProfileFromFiles 依据文件内容构造测试 profile(摘要由测试计算,
// 经仅测试可达的注册表变量注入)。
func testProfileFromFiles(files map[string]string) Profile {
	m := defaultManifest(files)
	p := Profile{ID: m.ProfileID, SourceRepository: m.Source.Repository,
		SourceTag: m.Source.Tag, SourceCommit: m.Source.Commit,
		Files: map[string]ProfileFile{}}
	for _, e := range m.Files {
		p.Files[e.ArchivePath] = ProfileFile{Purpose: e.Purpose, Size: e.SizeBytes, SHA256: e.SHA256}
	}
	return p
}

func registerTestProfile(t *testing.T, p Profile) {
	t.Helper()
	old, had := profileRegistry[p.ID]
	t.Cleanup(func() {
		if had {
			profileRegistry[p.ID] = old
		} else {
			delete(profileRegistry, p.ID)
		}
	})
	profileRegistry[p.ID] = p
}

// writeCInputDir 写出 c 输入目录;mutate 允许构造清单级破坏。
func writeCInputDir(t *testing.T, files map[string]string, mutate func(*manifest)) string {
	t.Helper()
	dir := t.TempDir()
	m := defaultManifest(files)
	if mutate != nil {
		mutate(&m)
	}
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inputs.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeCInput 写正例目录并注册对应测试 profile。
func writeCInput(t *testing.T, files map[string]string) (string, Profile) {
	t.Helper()
	prof := testProfileFromFiles(files)
	registerTestProfile(t, prof)
	return writeCInputDir(t, files, nil), prof
}

// mustReplace 对文本做一次替换;old 不存在即失败。
func mustReplace(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("text is missing the replacement target: %q", old)
	}
	return strings.Replace(s, old, new, 1)
}

func testProfile(t *testing.T) Profile {
	t.Helper()
	prof, ok := LookupProfile(testProfileID)
	if !ok {
		t.Fatal("test profile is not registered")
	}
	return prof
}

func testLoadCInput(t *testing.T, dir string) *CInput {
	t.Helper()
	input, diags := LoadCInput(dir, testProfile(t))
	if len(diags) != 0 {
		t.Fatalf("LoadCInput unexpectedly failed: %v", diags)
	}
	return input
}

// wantLoadFail 断言 LoadCInput 以 input 类诊断失败,且 Symbol 含期望片段。
func wantLoadFail(t *testing.T, dir, wantSymbol string) Diagnostic {
	t.Helper()
	_, diags := LoadCInput(dir, testProfile(t))
	if len(diags) == 0 {
		t.Fatalf("LoadCInput unexpectedly passed; want failure mentioning %q", wantSymbol)
	}
	d := diags[0]
	if d.Category != CatInput {
		t.Errorf("category = %q, want %q", d.Category, CatInput)
	}
	if wantSymbol != "" && !strings.Contains(d.Symbol, wantSymbol) {
		t.Errorf("symbol = %q, want it to contain %q", d.Symbol, wantSymbol)
	}
	return d
}

func TestLoadCInputPositive(t *testing.T) {
	files := positiveFiles(t)
	// 可容忍缺省的 semantic_reference/license 条目:存在即校验。
	files[FileAsstTypes] = "// synthetic semantic reference\n"
	files[FileLicense] = "synthetic license text\n"
	dir := writeCInputDir(t, files, nil)
	registerTestProfile(t, testProfileFromFiles(files))

	input := testLoadCInput(t, dir)
	if input.Dir != dir {
		t.Errorf("Dir = %q, want %q", input.Dir, dir)
	}
	if len(input.Contents) != 5 {
		t.Fatalf("Contents has %d files, want 5", len(input.Contents))
	}
	if string(input.Contents[FileAsstCaller]) != files[FileAsstCaller] {
		t.Error("caller content mismatch")
	}
	if _, ok := input.Contents[FileAsstMsg]; ok {
		t.Error("file absent from the manifest must not be read")
	}
}

func TestLoadCInputFileFailures(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir, _ := writeCInput(t, positiveFiles(t))
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(FileAsstPort))); err != nil {
			t.Fatal(err)
		}
		wantLoadFail(t, dir, "file")
	})
	t.Run("empty file", func(t *testing.T) {
		files := positiveFiles(t)
		files[FileAsstCallerExtra] = ""
		dir, _ := writeCInput(t, files)
		wantLoadFail(t, dir, "size")
	})
	t.Run("size mismatch", func(t *testing.T) {
		dir, _ := writeCInput(t, positiveFiles(t))
		full := filepath.Join(dir, filepath.FromSlash(FileAsstCaller))
		orig, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, append(orig, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		wantLoadFail(t, dir, "size")
	})
	t.Run("sha mismatch", func(t *testing.T) {
		dir, _ := writeCInput(t, positiveFiles(t))
		full := filepath.Join(dir, filepath.FromSlash(FileAsstCaller))
		orig, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		orig[0] ^= 0x10 // 同长度改写:只破坏摘要
		if err := os.WriteFile(full, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		wantLoadFail(t, dir, "sha256")
	})
}

func TestLoadCInputManifestFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*manifest)
		sym    string
	}{
		{"missing schema version", func(m *manifest) { m.SchemaVersion = nil }, "schema_version"},
		{"wrong schema version", func(m *manifest) { m.SchemaVersion = intPtr(2) }, "schema_version"},
		{"wrong profile id", func(m *manifest) { m.ProfileID = "other-profile" }, "profile_id"},
		{"wrong repository", func(m *manifest) { m.Source.Repository = "https://elsewhere.example" }, "source.repository"},
		{"wrong tag", func(m *manifest) { m.Source.Tag = "v0.0.0" }, "source.tag"},
		{"wrong commit", func(m *manifest) { m.Source.Commit = strings.Repeat("0", 40) }, "source.commit"},
		{"unknown entry", func(m *manifest) {
			m.Files = append(m.Files, manifestFile{ArchivePath: "upstream/Evil.h",
				Purpose: PurposeCheckerInput, SizeBytes: 1, SHA256: strings.Repeat("a", 64)})
		}, "Evil.h"},
		{"missing checker_input entry", func(m *manifest) {
			var out []manifestFile
			for _, e := range m.Files {
				if e.ArchivePath != FileAsstPort {
					out = append(out, e)
				}
			}
			m.Files = out
		}, FileAsstPort},
		{"purpose mismatch", func(m *manifest) {
			for i := range m.Files {
				if m.Files[i].ArchivePath == FileAsstCaller {
					m.Files[i].Purpose = PurposeSemanticReference
				}
			}
		}, "purpose"},
		{"manifest digest mismatch", func(m *manifest) {
			for i := range m.Files {
				if m.Files[i].ArchivePath == FileAsstCaller {
					m.Files[i].SHA256 = strings.Repeat("0", 64)
				}
			}
		}, "sha256"},
		{"manifest size mismatch", func(m *manifest) {
			for i := range m.Files {
				if m.Files[i].ArchivePath == FileAsstPort {
					m.Files[i].SizeBytes = 1
				}
			}
		}, "size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := positiveFiles(t)
			registerTestProfile(t, testProfileFromFiles(files))
			dir := writeCInputDir(t, files, tc.mutate)
			wantLoadFail(t, dir, tc.sym)
		})
	}
}

func TestLoadCInputPathValidation(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"absolute path", "/etc/evil.h"},
		{"parent escape", "../outside/AsstCaller.h"},
		{"inner parent", "upstream/../evil.h"},
		{"dot component", "./upstream/AsstCaller.h"},
		{"empty component", "upstream//AsstCaller.h"},
		{"empty path", ""},
		{"backslash", "upstream\\AsstCaller.h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := positiveFiles(t)
			registerTestProfile(t, testProfileFromFiles(files))
			dir := writeCInputDir(t, files, func(m *manifest) {
				for i := range m.Files {
					if m.Files[i].ArchivePath == FileAsstCaller {
						m.Files[i].ArchivePath = tc.path
					}
				}
			})
			d := wantLoadFail(t, dir, "archive_path")
			if d.Line != 0 {
				t.Errorf("manifest-level diagnostic should not carry a line, got %d", d.Line)
			}
		})
	}
	t.Run("duplicate entry", func(t *testing.T) {
		files := positiveFiles(t)
		registerTestProfile(t, testProfileFromFiles(files))
		dir := writeCInputDir(t, files, func(m *manifest) {
			m.Files = append(m.Files, m.Files[0]) // 任意条目重复
		})
		// 清单经排序后首个条目为 Caller,重复诊断指向该条目路径。
		wantLoadFail(t, dir, FileAsstCaller)
	})
}

// TestValidateRelPathUnits 直接覆盖路径校验的接受与拒绝集合。
func TestValidateRelPathUnits(t *testing.T) {
	for _, good := range []string{"upstream/AsstCaller.h", "a/b/c.h", "upstream/LICENSE"} {
		if d := validateRelPath(good); d != nil {
			t.Errorf("validateRelPath(%q) = %v, want nil", good, d)
		}
	}
	for i, bad := range []string{"", "/abs", "../x", "a/../b", "./a", "a//b", "a\\b", "a:b"} {
		if d := validateRelPath(bad); d == nil {
			t.Errorf("case %d: validateRelPath(%q) = nil, want failure", i, bad)
		}
	}
}

// TestLoadCInputMissingManifest 目录缺 inputs.json 时失败并定位到命令输入路径。
func TestLoadCInputMissingManifest(t *testing.T) {
	files := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(files))
	dir := t.TempDir()
	d := wantLoadFail(t, dir, "inputs.json")
	if !strings.Contains(d.File, "inputs.json") {
		t.Errorf("diag file = %q, want it to reference the manifest path", d.File)
	}
	if strconv.Itoa(manifestSchemaVersion) != "1" {
		t.Fatalf("frozen schema version changed: %d", manifestSchemaVersion)
	}
}
