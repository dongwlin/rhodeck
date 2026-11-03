package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
)

type RefIndex struct {
	protocol map[string]bool
	worker   map[string]bool
	client   map[string]bool
	tests    map[string]bool
}

func NewRefIndex(root string) (*RefIndex, error) {
	idx := &RefIndex{
		protocol: map[string]bool{},
		worker:   map[string]bool{},
		client:   map[string]bool{},
		tests:    map[string]bool{},
	}
	if err := idx.scanProtocol(filepath.Join(root, protocolDir)); err != nil {
		return nil, err
	}
	if err := idx.scanWorker(filepath.Join(root, workerDir)); err != nil {
		return nil, err
	}
	if err := idx.scanClient(filepath.Join(root, clientDir)); err != nil {
		return nil, err
	}
	for _, dir := range []string{
		filepath.Join(root, bindingDir),
		filepath.Join(root, nativeDir),
		filepath.Join(root, workerDir),
		filepath.Join(root, clientDir),
		filepath.Join(root, protocolDir),
	} {
		if err := idx.scanTests(dir); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func (idx *RefIndex) scanProtocol(dir string) error {
	return scanDecls(dir, func(file *ast.File) {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						idx.protocol[s.Name.Name] = true
					}
					if st, ok := s.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								if name.IsExported() {
									idx.protocol[s.Name.Name+"."+name.Name] = true
								}
							}
						}
					}
				case *ast.ValueSpec:
					if gd.Tok == token.CONST {
						for _, name := range s.Names {
							if name.IsExported() {
								idx.protocol[name.Name] = true
							}
						}
					}
				}
			}
		}
	})
}

func (idx *RefIndex) scanWorker(dir string) error {
	return scanDecls(dir, func(file *ast.File) {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Recv == nil || len(fd.Recv.List) == 0 {
				idx.worker[fd.Name.Name] = true
				continue
			}
			recv := receiverBase(fd.Recv.List[0].Type)
			if recv != "" {
				idx.worker[recv+"."+fd.Name.Name] = true
			}
		}
	})
}

func (idx *RefIndex) scanClient(dir string) error {
	return scanDecls(dir, func(file *ast.File) {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Recv == nil || len(fd.Recv.List) == 0 {
				idx.client[fd.Name.Name] = true
				continue
			}
			recv := receiverBase(fd.Recv.List[0].Type)
			if recv != "" {
				idx.client[recv+"."+fd.Name.Name] = true
			}
		}
	})
}

func (idx *RefIndex) scanTests(dir string) error {
	return scanDecls(dir, func(file *ast.File) {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			if strings.HasPrefix(fd.Name.Name, "Test") {
				idx.tests[fd.Name.Name] = true
			}
		}
	}, testFilesOnly)
}

type fileFilter int

const (
	nonTestFiles fileFilter = iota
	testFilesOnly
	allGoFiles
)

func scanDecls(dir string, fn func(*ast.File), filters ...fileFilter) error {
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return err
	}
	sort.Strings(entries)
	filter := nonTestFiles
	if len(filters) > 0 {
		filter = filters[0]
	}
	var have bool
	fset := token.NewFileSet()
	for _, path := range entries {
		base := filepath.Base(path)
		isTest := strings.HasSuffix(base, "_test.go")
		switch filter {
		case nonTestFiles:
			have = !isTest
		case testFilesOnly:
			have = isTest
		case allGoFiles:
			have = true
		}
		if !have {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		fn(file)
	}
	return nil
}

func (idx *RefIndex) resolveRefs(segment string, refs string) []string {
	var missing []string
	for _, ref := range strings.Split(refs, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		var ok bool
		switch segment {
		case "dto":
			ok = idx.protocol[ref]
		case "worker":
			ok = idx.worker[ref]
		case "client":
			ok = idx.client[ref]
		case "test":
			ok = idx.tests[ref]
		}
		if !ok {
			missing = append(missing, ref)
		}
	}
	return missing
}
