//go:build linux

package maa

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func coreInputEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writeCoreInputFile(t *testing.T, root, name, data string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoreInputInstalledAndOptionalDigests(t *testing.T) {
	root := t.TempDir()
	lib := writeCoreInputFile(t, root, coreSOName, "explicit library input")
	version := writeCoreInputFile(t, root, versionRelPath, `{"version":"rolling"}`)
	libSHA, err := sha256File(lib)
	if err != nil {
		t.Fatal(err)
	}
	versionSHA, err := sha256File(version)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{libPathEnv: lib, resourceRootEnv: root, expectedLibrarySHAEnv: strings.ToUpper(libSHA), expectedVersionSHAEnv: versionSHA}
	r, err := prepareCoreResources(coreInputEnv(values))
	if err != nil {
		t.Fatal(err)
	}
	if r.root != root || r.libPath != lib || r.cleanupRoot != "" || r.librarySHA != libSHA || r.versionSHA != versionSHA || r.tarballSHA != "" {
		t.Fatalf("prepared installation = %+v", r)
	}
	values[expectedLibrarySHAEnv] = strings.Repeat("0", 64)
	if _, err := prepareCoreResources(coreInputEnv(values)); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("library mismatch = %v", err)
	}
	values[expectedLibrarySHAEnv] = libSHA
	values[expectedVersionSHAEnv] = strings.Repeat("0", 64)
	if _, err := prepareCoreResources(coreInputEnv(values)); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("version file mismatch = %v", err)
	}
}

func TestCoreInputMissingAndInvalid(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name    string
		env     map[string]string
		missing bool
	}{
		{name: "none", missing: true},
		{name: "incomplete with invalid library digest", env: map[string]string{expectedLibrarySHAEnv: "bad"}},
		{name: "incomplete with invalid version digest", env: map[string]string{expectedVersionSHAEnv: "bad"}},
		{name: "incomplete with tarball expectation", env: map[string]string{expectedTarballSHAEnv: strings.Repeat("0", 64)}},
		{name: "incomplete with valid library expectation", env: map[string]string{expectedLibrarySHAEnv: strings.Repeat("0", 64)}, missing: true},
		{name: "library only", env: map[string]string{libPathEnv: "/explicit/libMaaCore.so"}, missing: true},
		{name: "resource only", env: map[string]string{resourceRootEnv: root}, missing: true},
		{name: "conflicting sources", env: map[string]string{libPathEnv: "/explicit/libMaaCore.so", tarballOverrideEv: "/explicit/core.tar.gz"}},
		{name: "relative installation", env: map[string]string{libPathEnv: "libMaaCore.so", resourceRootEnv: root}},
		{name: "missing installed file", env: map[string]string{libPathEnv: filepath.Join(root, coreSOName), resourceRootEnv: root}},
		{name: "missing archive", env: map[string]string{tarballOverrideEv: filepath.Join(root, "absent.tar.gz")}},
		{name: "relative archive", env: map[string]string{tarballOverrideEv: "core.tar.gz"}},
		{name: "bad digest", env: map[string]string{libPathEnv: "/explicit/libMaaCore.so", resourceRootEnv: root, expectedLibrarySHAEnv: "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := prepareCoreResources(coreInputEnv(tc.env))
			if err == nil || errors.Is(err, errCoreInputMissing) != tc.missing {
				t.Fatalf("preparation error = %v, missing = %v", err, tc.missing)
			}
		})
	}
}

type coreArchiveMember struct {
	name, data, link string
	kind             byte
}

func writeCoreInputArchive(t *testing.T, members []coreArchiveMember) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "core.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		kind := m.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		hdr := &tar.Header{Name: m.name, Mode: 0o600, Typeflag: kind, Linkname: m.link}
		if kind == tar.TypeReg {
			hdr.Size = int64(len(m.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(m.data)); err != nil {
			t.Fatal(err)
		}
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestCoreInputExplicitArchive(t *testing.T) {
	archive := writeCoreInputArchive(t, []coreArchiveMember{
		{name: "./", kind: tar.TypeDir},
		{name: coreSOName, data: "supplied library"},
		{name: "libDependency.so.1", data: "dependency"},
		{name: "libDependency.so", kind: tar.TypeSymlink, link: "libDependency.so.1"},
		{name: versionRelPath, data: `{"version":"new"}`},
		{name: "resource/nested/data.json", data: "{}"},
		{name: "AsstCaller.h", data: "ignored"},
	})
	digest, err := sha256File(archive)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{tarballOverrideEv: archive, expectedTarballSHAEnv: digest}
	r, err := prepareCoreResources(coreInputEnv(values))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(r.cleanupRoot)
	if r.cleanupRoot == "" || r.tarballSHA != digest || r.libPath != filepath.Join(r.root, coreSOName) {
		t.Fatalf("prepared archive = %+v", r)
	}
	if got, err := os.ReadFile(filepath.Join(r.root, "libDependency.so")); err != nil || string(got) != "dependency" {
		t.Fatalf("dependency symlink = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(r.root, "AsstCaller.h")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unselected member error = %v", err)
	}
	values[expectedTarballSHAEnv] = strings.Repeat("0", 64)
	if _, err := prepareCoreResources(coreInputEnv(values)); err == nil {
		t.Fatal("archive digest mismatch accepted")
	}
}

func TestCoreInputRejectsUnsafeOrIncompleteArchive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []coreArchiveMember
	}{
		{"root regular file", []coreArchiveMember{{name: ".", data: "bad"}}},
		{"missing version", []coreArchiveMember{{name: coreSOName, data: "library"}}},
		{"missing library", []coreArchiveMember{{name: versionRelPath, data: "{}"}}},
		{"parent traversal", []coreArchiveMember{{name: "resource/../../escape", data: "bad"}}},
		{"directory traversal", []coreArchiveMember{{name: "resource/../../escape", kind: tar.TypeDir}}},
		{"absolute path", []coreArchiveMember{{name: "/resource/escape", data: "bad"}}},
		{"duplicate", []coreArchiveMember{{name: coreSOName, data: "first"}, {name: coreSOName, data: "second"}}},
		{"symlink traversal", []coreArchiveMember{{name: "libDependency.so", kind: tar.TypeSymlink, link: "../escape.so"}}},
		{"resource symlink", []coreArchiveMember{{name: "resource/link", kind: tar.TypeSymlink, link: "data"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := writeCoreInputArchive(t, tc.members)
			r, err := prepareCoreResources(coreInputEnv(map[string]string{tarballOverrideEv: archive}))
			if err == nil {
				defer os.RemoveAll(r.cleanupRoot)
				t.Fatal("invalid archive accepted")
			}
			if r.cleanupRoot != "" {
				if _, statErr := os.Stat(r.cleanupRoot); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed extraction was not removed: %v", statErr)
				}
			}
		})
	}
}

func TestCoreMetadataRollingVersionAndABISentinel(t *testing.T) {
	t.Setenv(expectedVersionEnv, "")
	good := fmt.Sprintf("%s version=v99.0.0 nullSize=%d\n", successMarker, uint64(math.MaxUint64))
	if err := verifySuccessChildMetadata(good); err != nil {
		t.Fatal(err)
	}
	if err := verifyCoreVersion("v99.0.0", "v99.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, stdout := range []string{"", strings.Replace(good, "version=v99.0.0", "version=", 1), strings.Replace(good, fmt.Sprint(uint64(math.MaxUint64)), "0", 1)} {
		if err := verifySuccessChildMetadata(stdout); err == nil {
			t.Fatalf("invalid metadata accepted: %q", stdout)
		}
	}
	t.Setenv(expectedVersionEnv, "v100.0.0")
	if err := verifySuccessChildMetadata(good); err == nil {
		t.Fatal("explicit version mismatch accepted")
	}
}
