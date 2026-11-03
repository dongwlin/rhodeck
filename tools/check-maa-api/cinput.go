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

const manifestSchemaVersion = 1

type manifestSource struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Commit     string `json:"commit,omitempty"`
}

type manifestFile struct {
	ArchivePath string `json:"archive_path"`
	SourcePath  string `json:"source_path"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Purpose     string `json:"purpose"`
	ReleasePath string `json:"release_path,omitempty"`
}

type manifest struct {
	SchemaVersion *int           `json:"schema_version"`
	SnapshotID    string         `json:"snapshot_id"`
	Source        manifestSource `json:"source"`
	Files         []manifestFile `json:"files"`
}

type CInput struct {
	Dir      string
	Profile  Profile
	Contents map[string][]byte
	Manifest manifest
}

func LoadCInput(dir string, expected ...Profile) (*CInput, []Diagnostic) {
	manifestPath := filepath.Join(dir, "inputs.json")
	fail := func(symbol, want, got, message string) (*CInput, []Diagnostic) {
		return nil, []Diagnostic{{Category: CatInput, File: manifestPath, Symbol: symbol, Expected: want, Actual: got, Message: message}}
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fail("inputs.json", "readable inputs.json", err.Error(), "c input directory must contain inputs.json")
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail("inputs.json", "valid JSON manifest", err.Error(), "inputs.json is not a valid manifest")
	}
	if m.SchemaVersion == nil {
		return fail("schema_version", strconv.Itoa(manifestSchemaVersion), "missing", "manifest schema_version is required")
	}
	if *m.SchemaVersion != manifestSchemaVersion {
		return fail("schema_version", strconv.Itoa(manifestSchemaVersion), strconv.Itoa(*m.SchemaVersion), "unsupported manifest schema version")
	}
	if strings.TrimSpace(m.SnapshotID) == "" {
		return fail("snapshot_id", "non-empty snapshot identity", m.SnapshotID, "manifest snapshot_id is required")
	}
	if strings.TrimSpace(m.Source.Repository) == "" {
		return fail("source.repository", "recorded source repository or download URL", m.Source.Repository, "manifest source is required")
	}
	if strings.TrimSpace(m.Source.Tag) == "" && strings.TrimSpace(m.Source.Commit) == "" {
		return fail("source", "source tag or commit", "missing", "manifest source revision is required")
	}
	profile := Profile{ID: m.SnapshotID, SourceRepository: m.Source.Repository, SourceTag: m.Source.Tag, SourceCommit: m.Source.Commit, Files: map[string]ProfileFile{}}
	if len(expected) > 0 {
		p := expected[0]
		for _, check := range []struct{ symbol, want, got string }{
			{"snapshot_id", p.ID, m.SnapshotID}, {"source.repository", p.SourceRepository, m.Source.Repository}, {"source.tag", p.SourceTag, m.Source.Tag}, {"source.commit", p.SourceCommit, m.Source.Commit},
		} {
			if check.want != check.got {
				return fail(check.symbol, check.want, check.got, "manifest does not match the explicit input expectation")
			}
		}
	}
	purposes := map[string]string{FileAsstCaller: PurposeCheckerInput, FileAsstPort: PurposeCheckerInput, FileAsstCallerExtra: PurposeCheckerInput, FileAsstTypes: PurposeSemanticReference, FileAsstMsg: PurposeSemanticReference, FileLicense: PurposeUpstreamLicense}
	for _, e := range m.Files {
		if d := validateRelPath(e.ArchivePath); d != nil {
			d.Category = CatInput
			d.File = manifestPath
			d.Symbol = "archive_path"
			return nil, []Diagnostic{*d}
		}
		if _, seen := profile.Files[e.ArchivePath]; seen {
			return fail(e.ArchivePath, "unique archive_path", "duplicate entry", "manifest lists the same archive_path twice")
		}
		purpose, ok := purposes[e.ArchivePath]
		if !ok {
			return fail(e.ArchivePath, "known input or reference file", "unknown entry", "manifest contains an unknown file")
		}
		if e.Purpose != purpose {
			return fail(e.ArchivePath+" purpose", purpose, e.Purpose, "manifest purpose mismatch")
		}
		decoded, err := hex.DecodeString(e.SHA256)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(e.SHA256) != e.SHA256 {
			return fail(e.ArchivePath+" sha256", "64 lowercase hexadecimal digits", e.SHA256, "invalid manifest SHA256")
		}
		if e.SizeBytes <= 0 {
			return fail(e.ArchivePath+" size", "positive size_bytes", strconv.FormatInt(e.SizeBytes, 10), "input files must be non-empty")
		}
		entry := ProfileFile{Purpose: e.Purpose, Size: e.SizeBytes, SHA256: e.SHA256}
		if len(expected) > 0 {
			want, ok := expected[0].Files[e.ArchivePath]
			if !ok {
				return fail(e.ArchivePath, "expected file", "unknown entry", "manifest file absent from explicit expectation")
			}
			if entry.SHA256 != want.SHA256 {
				return fail(e.ArchivePath+" sha256", want.SHA256, entry.SHA256, "manifest digest mismatch")
			}
			if entry.Size != want.Size {
				return fail(e.ArchivePath+" size", strconv.FormatInt(want.Size, 10), strconv.FormatInt(entry.Size, 10), "manifest size mismatch")
			}
		}
		profile.Files[e.ArchivePath] = entry
	}
	for _, key := range []string{FileAsstCaller, FileAsstPort, FileAsstCallerExtra} {
		if _, ok := profile.Files[key]; !ok {
			return fail(key, "manifest entry present", "missing", "required checker_input file is missing from the manifest")
		}
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fail("c-input", "accessible directory", err.Error(), "cannot resolve c input directory")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return fail("c-input", "absolute input directory", err.Error(), "cannot resolve c input directory")
	}
	contents := make(map[string][]byte, len(profile.Files))
	for _, key := range sortedProfileFiles(profile) {
		want := profile.Files[key]
		full := filepath.Join(dir, filepath.FromSlash(key))
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			return fail("file", key, err.Error(), "archived file missing on disk: "+full)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return fail("file", key, err.Error(), "cannot resolve archived file")
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fail(key, "file within input directory", resolved, "archived file escapes input directory")
		}
		st, err := os.Stat(resolved)
		if err != nil || !st.Mode().IsRegular() {
			return fail("file", key, "not a regular file", "archived path is not a regular file: "+full)
		}
		if st.Size() != want.Size {
			return fail("size", strconv.FormatInt(want.Size, 10), strconv.FormatInt(st.Size(), 10), "archived file size mismatch: "+key)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return fail("file", key, err.Error(), "archived file cannot be read")
		}
		if int64(len(data)) != want.Size {
			return fail("size", strconv.FormatInt(want.Size, 10), strconv.Itoa(len(data)), "archived file size mismatch: "+key)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != want.SHA256 {
			return fail("sha256", want.SHA256, got, "archived file digest mismatch: "+key)
		}
		contents[key] = data
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].ArchivePath < m.Files[j].ArchivePath })
	return &CInput{Dir: dir, Profile: profile, Contents: contents, Manifest: m}, nil
}

func sortedProfileFiles(p Profile) []string {
	keys := make([]string, 0, len(p.Files))
	for k := range p.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
			return &Diagnostic{Expected: "path without empty components", Actual: p, Message: "archive_path contains an empty path component"}
		case comp == "." || comp == "..":
			return &Diagnostic{Expected: "path without . or .. components", Actual: p, Message: "archive_path must stay inside the c input directory"}
		case strings.ContainsAny(comp, "\\/:"):
			return &Diagnostic{Expected: "plain path component", Actual: comp, Message: "archive_path contains a forbidden character"}
		}
	}
	return nil
}
