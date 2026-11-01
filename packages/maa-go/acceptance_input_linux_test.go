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

	"github.com/stretchr/testify/require"
)

func coreInputEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func writeCoreInputFile(t *testing.T, root, name, data string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		require.NoError(t, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		require.NoError(t, err)
	}
	return path
}

func TestCoreInputInstalledAndOptionalDigests(t *testing.T) {
	root := t.TempDir()
	lib := writeCoreInputFile(t, root, coreSOName, "explicit library input")
	version := writeCoreInputFile(t, root, versionRelPath, `{"version":"rolling"}`)
	libSHA, err := sha256File(lib)
	require.NoError(t, err)
	versionSHA, err := sha256File(version)
	require.NoError(t, err)
	values := map[string]string{libPathEnv: lib, resourceRootEnv: root, expectedLibrarySHAEnv: strings.ToUpper(libSHA), expectedVersionSHAEnv: versionSHA}
	r, err := prepareCoreResources(coreInputEnv(values))
	require.NoError(t, err)
	require.True(t, r.root == root && r.libPath == lib && r.cleanupRoot == "" && r.librarySHA == libSHA && r.versionSHA == versionSHA && r.tarballSHA == "", "prepared installation = %+v", r)
	values[expectedLibrarySHAEnv] = strings.Repeat("0", 64)
	_, err = prepareCoreResources(coreInputEnv(values))
	require.Error(t, err, "library mismatch = %v", err)
	require.Contains(t, err.Error(), "SHA256", "library mismatch = %v", err)
	values[expectedLibrarySHAEnv] = libSHA
	values[expectedVersionSHAEnv] = strings.Repeat("0", 64)
	_, err = prepareCoreResources(coreInputEnv(values))
	require.Error(t, err, "version file mismatch = %v", err)
	require.Contains(t, err.Error(), "SHA256", "version file mismatch = %v", err)
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
			require.True(t, err != nil && errors.Is(err, errCoreInputMissing) == tc.missing, "preparation error = %v, missing = %v", err, tc.missing)
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
		require.NoError(t, err)
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
			require.NoError(t, err)
		}
		if _, err := tw.Write([]byte(m.data)); err != nil {
			require.NoError(t, err)
		}
	}
	for _, close := range []func() error{tw.Close, gz.Close, f.Close} {
		if err := close(); err != nil {
			require.NoError(t, err)
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
	require.NoError(t, err)
	values := map[string]string{tarballOverrideEv: archive, expectedTarballSHAEnv: digest}
	r, err := prepareCoreResources(coreInputEnv(values))
	require.NoError(t, err)
	defer os.RemoveAll(r.cleanupRoot)
	require.True(t, r.cleanupRoot != "" && r.tarballSHA == digest && r.libPath == filepath.Join(r.root, coreSOName), "prepared archive = %+v", r)
	got, err := os.ReadFile(filepath.Join(r.root, "libDependency.so"))
	require.True(t, err == nil && string(got) == "dependency", "dependency symlink = %q, %v", got, err)
	_, err = os.Stat(filepath.Join(r.root, "AsstCaller.h"))
	require.ErrorIs(t, err, os.ErrNotExist, "unselected member error = %v", err)
	values[expectedTarballSHAEnv] = strings.Repeat("0", 64)
	_, err = prepareCoreResources(coreInputEnv(values))
	require.Error(t, err, "archive digest mismatch accepted")
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
				require.Fail(t, "invalid archive accepted")
			}
			if r.cleanupRoot != "" {
				_, statErr := os.Stat(r.cleanupRoot)
				require.ErrorIs(t, statErr, os.ErrNotExist, "failed extraction was not removed: %v", statErr)
			}
		})
	}
}

func TestCoreMetadataRollingVersionAndABISentinel(t *testing.T) {
	t.Setenv(expectedVersionEnv, "")
	good := fmt.Sprintf("%s version=v99.0.0 nullSize=%d\n", successMarker, uint64(math.MaxUint64))
	require.NoError(t, verifySuccessChildMetadata(good))
	require.NoError(t, verifyCoreVersion("v99.0.0", "v99.0.0"))
	for _, stdout := range []string{"", strings.Replace(good, "version=v99.0.0", "version=", 1), strings.Replace(good, fmt.Sprint(uint64(math.MaxUint64)), "0", 1)} {
		require.Error(t, verifySuccessChildMetadata(stdout), "invalid metadata accepted: %q", stdout)
	}
	t.Setenv(expectedVersionEnv, "v100.0.0")
	require.Error(t, verifySuccessChildMetadata(good), "explicit version mismatch accepted")
}
