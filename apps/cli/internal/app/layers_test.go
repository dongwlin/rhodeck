package app

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// allowedLayers is the layering of the CLI. Every package depends on the
// packages below it and on nothing else. The table keeps the dependency
// direction enforceable instead of documented.
var allowedLayers = map[string][]string{
	"app":    {"cmd", "output"},
	"cmd":    {"plan", "output"},
	"plan":   {"output", "control"},
	"output": {"control"},
}

// TestInternalLayeringStaysStrict keeps the CLI packages on their layer. A
// package may only import the packages listed for it; every other repository
// package is a layering violation.
func TestInternalLayeringStaysStrict(t *testing.T) {
	for pkg, allowed := range allowedLayers {
		t.Run(pkg, func(t *testing.T) {
			for _, imported := range packageImports(t, filepath.Join("..", pkg)) {
				layer := layerOf(imported)
				require.NotEmpty(t, layer, "package %s imports the repository package %s", pkg, imported)
				require.Contains(t, allowed, layer, "package %s must not import %s", pkg, imported)
			}
		})
	}
}

// layerOf names the layer of one repository import.
func layerOf(path string) string {
	switch {
	case strings.HasPrefix(path, "github.com/dongwlin/rhodeck/apps/cli/internal/"):
		return strings.TrimPrefix(path, "github.com/dongwlin/rhodeck/apps/cli/internal/")
	case strings.HasPrefix(path, "github.com/dongwlin/rhodeck/packages/control"):
		return "control"
	default:
		return ""
	}
}

// packageImports returns the imports of the production files of one package.
func packageImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	set := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ImportsOnly)
		require.NoError(t, err, "parse %s", name)
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if !strings.HasPrefix(path, "github.com/dongwlin/rhodeck/") {
				continue
			}
			set[path] = true
		}
	}
	imports := make([]string, 0, len(set))
	for path := range set {
		imports = append(imports, path)
	}
	return imports
}
