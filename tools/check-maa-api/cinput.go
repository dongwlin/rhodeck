// cinput.go 校验独立 C 输入目录:目录须含 inputs.json 及其相对路径
// 文件。schema_version、profile_id、source.repository/tag/commit 必须
// 与 profile 内部常量一致;manifest 条目集合与 profile 已知文件一致
// (未知条目失败;三个 checker_input 缺一失败;semantic_reference/
// license 条目缺省可容忍但存在即校验)。每个 archive_path 必须相对、
// 非空、不含.. 分量、不重复;文件存在、非空,size 与 SHA-256 以
// checker 内部常量为期望值,清单自带摘要只用于交叉核对。

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
)

// manifestSchemaVersion 是冻结的清单 schema 版本。
const manifestSchemaVersion = 1

type manifestSource struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
}

type manifestFile struct {
	ArchivePath string `json:"archive_path"`
	SourcePath  string `json:"source_path"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Purpose     string `json:"purpose"`
	ReleasePath string `json:"release_path"`
}

type manifest struct {
	SchemaVersion *int           `json:"schema_version"`
	ProfileID     string         `json:"profile_id"`
	Source        manifestSource `json:"source"`
	Files         []manifestFile `json:"files"`
}

// CInput 是通过校验的 c 输入:Contents 以归档相对路径为键保存已
// 核对大小与摘要的文件原文,提取阶段只读取这些字节,不再触碰磁盘。
type CInput struct {
	Dir      string
	Profile  Profile
	Contents map[string][]byte
}

// LoadCInput 校验并读取 c 输入目录;任何标识不符、路径越界、缺项、
// 空文件、大小或摘要不符都以 input 类诊断失败。
func LoadCInput(dir string, profile Profile) (*CInput, []Diagnostic) {
	manifestPath := filepath.Join(dir, "inputs.json")
	fail := func(d Diagnostic) (*CInput, []Diagnostic) { return nil, []Diagnostic{d} }

	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "inputs.json",
			Expected: "readable inputs.json", Actual: err.Error(),
			Message: "c input directory must contain inputs.json"})
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "inputs.json",
			Expected: "valid JSON manifest", Actual: err.Error(),
			Message: "inputs.json is not a valid manifest"})
	}
	if m.SchemaVersion == nil {
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "schema_version",
			Expected: strconv.Itoa(manifestSchemaVersion), Actual: "missing",
			Message: "manifest schema_version is required"})
	}
	if *m.SchemaVersion != manifestSchemaVersion {
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "schema_version",
			Expected: strconv.Itoa(manifestSchemaVersion), Actual: strconv.Itoa(*m.SchemaVersion),
			Message: "unsupported manifest schema version"})
	}
	if m.ProfileID != profile.ID {
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "profile_id",
			Expected: profile.ID, Actual: m.ProfileID, Message: "manifest profile mismatch"})
	}
	switch {
	case m.Source.Repository != profile.SourceRepository:
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "source.repository",
			Expected: profile.SourceRepository, Actual: m.Source.Repository, Message: "manifest source mismatch"})
	case m.Source.Tag != profile.SourceTag:
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "source.tag",
			Expected: profile.SourceTag, Actual: m.Source.Tag, Message: "manifest source mismatch"})
	case m.Source.Commit != profile.SourceCommit:
		return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: "source.commit",
			Expected: profile.SourceCommit, Actual: m.Source.Commit, Message: "manifest source mismatch"})
	}

	seen := map[string]bool{}
	for _, e := range m.Files {
		if d := validateRelPath(e.ArchivePath); d != nil {
			d.Category = CatInput
			d.File = manifestPath
			d.Symbol = "archive_path"
			return fail(*d)
		}
		if seen[e.ArchivePath] {
			return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: e.ArchivePath,
				Expected: "unique archive_path", Actual: "duplicate entry",
				Message: "manifest lists the same archive_path twice"})
		}
		seen[e.ArchivePath] = true
		expected, ok := profile.Files[e.ArchivePath]
		if !ok {
			return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: e.ArchivePath,
				Expected: "entry absent (unknown to profile)", Actual: "present",
				Message: "manifest contains an entry unknown to the locked profile"})
		}
		if e.Purpose != expected.Purpose {
			return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: e.ArchivePath + " purpose",
				Expected: expected.Purpose, Actual: e.Purpose, Message: "manifest purpose mismatch"})
		}
		if e.SHA256 != expected.SHA256 {
			return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: e.ArchivePath + " sha256",
				Expected: expected.SHA256, Actual: e.SHA256,
				Message: "manifest digest does not match the checker-internal constant"})
		}
		if e.SizeBytes != expected.Size {
			return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: e.ArchivePath + " size",
				Expected: strconv.FormatInt(expected.Size, 10), Actual: strconv.FormatInt(e.SizeBytes, 10),
				Message: "manifest size does not match the checker-internal constant"})
		}
	}
	for _, key := range sortedProfileFiles(profile) {
		if profile.Files[key].Purpose == PurposeCheckerInput && !seen[key] {
			return fail(Diagnostic{Category: CatInput, File: manifestPath, Symbol: key,
				Expected: "manifest entry present", Actual: "missing",
				Message: "required checker_input file is missing from the manifest"})
		}
	}

	contents := make(map[string][]byte, len(profile.Files))
	for _, key := range sortedProfileFiles(profile) {
		if !seen[key] {
			continue // 可容忍缺省的 semantic_reference/license 条目
		}
		expected := profile.Files[key]
		full := filepath.Join(dir, filepath.FromSlash(key))
		st, err := os.Stat(full)
		if err != nil {
			return fail(Diagnostic{Category: CatInput, File: key, Symbol: "file",
				Expected: "existing file", Actual: "missing",
				Message: "archived file missing on disk: " + full})
		}
		if !st.Mode().IsRegular() {
			return fail(Diagnostic{Category: CatInput, File: key, Symbol: "file",
				Expected: "regular file", Actual: "not a regular file",
				Message: "archived path is not a regular file: " + full})
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return fail(Diagnostic{Category: CatInput, File: key, Symbol: "file",
				Expected: "readable file", Actual: err.Error(),
				Message: "archived file cannot be read: " + full})
		}
		if len(data) == 0 {
			return fail(Diagnostic{Category: CatInput, File: key, Symbol: "size",
				Expected: strconv.FormatInt(expected.Size, 10), Actual: "0",
				Message: "archived file is empty: " + full})
		}
		if int64(len(data)) != expected.Size {
			return fail(Diagnostic{Category: CatInput, File: key, Symbol: "size",
				Expected: strconv.FormatInt(expected.Size, 10), Actual: strconv.Itoa(len(data)),
				Message: "archived file size mismatch"})
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != expected.SHA256 {
			return fail(Diagnostic{Category: CatInput, File: key, Symbol: "sha256",
				Expected: expected.SHA256, Actual: got,
				Message: "archived file digest mismatch against the checker-internal constant"})
		}
		contents[key] = data
	}
	return &CInput{Dir: dir, Profile: profile, Contents: contents}, nil
}

func sortedProfileFiles(p Profile) []string {
	keys := make([]string, 0, len(p.Files))
	for k := range p.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateRelPath 校验归档相对路径:非空、相对、无空分量、无. 或
// .. 分量、不含反斜杠或冒号;失败返回未定类别的诊断(由调用方补齐)。
func validateRelPath(p string) *Diagnostic {
	switch {
	case p == "":
		return &Diagnostic{Expected: "non-empty relative path", Actual: `""`, Message: "archive_path is empty"}
	case strings.HasPrefix(p, "/"):
		return &Diagnostic{Expected: "relative path", Actual: p, Message: "archive_path must not be absolute"}
	}
	for _, comp := range strings.Split(p, "/") {
		switch {
		case comp == "":
			return &Diagnostic{Expected: "path without empty components", Actual: p,
				Message: "archive_path contains an empty path component"}
		case comp == "." || comp == "..":
			return &Diagnostic{Expected: "path without . or .. components", Actual: p,
				Message: "archive_path must stay inside the c input directory"}
		case strings.ContainsAny(comp, "\\/:"):
			return &Diagnostic{Expected: "plain path component", Actual: comp,
				Message: "archive_path contains a forbidden character"}
		}
	}
	return nil
}
