package main

import (
	"fmt"
	"go/build"
	"go/build/constraint"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const goNativeRelPath = "packages/maa-go/internal/native"

var sharedDeclFiles = []string{"api.go", "symbols.go", "callback_types.go"}

type GoSourceFile struct {
	Name string
	Path string
}

type GoSelection struct {
	Dir   string
	Files []GoSourceFile
}

func (s *GoSelection) names() []string {
	out := make([]string, len(s.Files))
	for i, f := range s.Files {
		out[i] = f.Name
	}
	return out
}

func (s *GoSelection) contains(name string) bool {
	for _, f := range s.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}

func SelectGoFiles(goRoot string, target Target) (*GoSelection, []Diagnostic) {
	fail := func(d Diagnostic) (*GoSelection, []Diagnostic) { return nil, []Diagnostic{d} }
	dir := filepath.Join(goRoot, filepath.FromSlash(goNativeRelPath))
	st, err := os.Stat(dir)
	if err != nil {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: goNativeRelPath,
			Expected: "existing go package directory", Actual: "missing",
			Message: "go input directory does not exist under --go-root"})
	}
	if !st.IsDir() {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: goNativeRelPath,
			Expected: "directory", Actual: "not a directory",
			Message: "go input path is not a directory"})
	}

	ctx := build.Context{
		GOOS:        target.GOOS,
		GOARCH:      target.GOARCH,
		GOROOT:      runtime.GOROOT(),
		GOPATH:      build.Default.GOPATH,
		Dir:         build.Default.Dir,
		Compiler:    build.Default.Compiler,
		CgoEnabled:  false,
		ReleaseTags: build.Default.ReleaseTags,
	}
	pkg, err := ctx.ImportDir(dir, build.IgnoreVendor)
	if err != nil {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: "go files",
			Expected: "buildable go package", Actual: err.Error(),
			Message: "go input directory does not select any buildable file for this target"})
	}
	if len(pkg.GoFiles) == 0 {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: "go files",
			Expected: "at least one selected go file", Actual: "none",
			Message: "no go file participates in the target build"})
	}

	names := append([]string(nil), pkg.GoFiles...)
	sort.Strings(names)
	sel := &GoSelection{Dir: dir}
	for _, name := range names {
		sel.Files = append(sel.Files, GoSourceFile{Name: name, Path: filepath.Join(dir, name)})
	}

	for _, name := range sharedDeclFiles {
		full := filepath.Join(dir, name)
		st, err := os.Stat(full)
		switch {
		case err != nil || st.IsDir():
			return fail(Diagnostic{Category: CatInput, File: full, Symbol: name,
				Expected: "existing shared declaration file", Actual: "missing",
				Message: "frozen shared declaration file is missing"})
		case !sel.contains(name):
			return fail(Diagnostic{Category: CatInput, File: full, Symbol: name,
				Expected: "selected for the target build", Actual: "hidden by build constraints",
				Message: "shared declaration file must not carry build constraints; it exists but is not selected"})
		}
		if line, kind := firstBuildConstraint(full); line > 0 {
			return fail(Diagnostic{Category: CatGoSyntax, File: full, Line: line, Symbol: name,
				Expected: "shared declaration file without build constraints", Actual: kind,
				Message: "shared declaration file must be unconditionally part of every target"})
		}
	}
	return sel, nil
}

func firstBuildConstraint(path string) (int, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	for i, ln := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			if constraint.IsGoBuild(trimmed) {
				return i + 1, trimmed
			}
			if constraint.IsPlusBuild(trimmed) {
				return i + 1, trimmed
			}
			continue
		}
		if strings.HasPrefix(trimmed, "package ") || trimmed == "package" {
			return 0, ""
		}
		if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		return 0, ""
	}
	return 0, ""
}

func goRootAbs(goRoot string) (string, *Diagnostic) {
	abs, err := filepath.Abs(goRoot)
	if err != nil {
		return "", &Diagnostic{Category: CatInput, File: goRoot, Symbol: "--go-root",
			Expected: "resolvable go root path", Actual: err.Error(),
			Message: "--go-root cannot be resolved to an absolute path"}
	}
	return abs, nil
}

func fmtTarget(t Target) string {
	return fmt.Sprintf("(%s, %s)", t.GOOS, t.GOARCH)
}
