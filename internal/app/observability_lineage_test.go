//go:build integration

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestObservability_VersionLineageOrigins 在版本数值偶合时仍守住后端注入来源。
// TestObservability_VersionLineageOrigins guards backend origins even when version integers coincide.
func TestObservability_VersionLineageOrigins(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	require.NoError(t, err)
	imports := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		} else {
			for i := len(path) - 1; i >= 0; i-- {
				if path[i] == '/' {
					name = path[i+1:]
					break
				}
			}
		}
		imports[name] = path
	}
	want := map[string]string{"SupportedSchemaVersion": "github.com/ymedword/basalt/internal/storage/postgres", "SourceSchemaVersion": "github.com/ymedword/basalt/internal/storage"}
	found := map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typ, ok := literal.Type.(*ast.SelectorExpr)
		if !ok || typ.Sel.Name != "Deps" {
			return true
		}
		pkg, ok := typ.X.(*ast.Ident)
		if !ok || imports[pkg.Name] != "github.com/ymedword/basalt/internal/cli" {
			return true
		}
		for _, element := range literal.Elts {
			pair, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := pair.Key.(*ast.Ident)
			if !ok {
				continue
			}
			path, needed := want[key.Name]
			if !needed {
				continue
			}
			value, ok := pair.Value.(*ast.SelectorExpr)
			require.True(t, ok, "version origin must remain an explicit backend constant")
			origin, ok := value.X.(*ast.Ident)
			require.True(t, ok)
			require.Equal(t, "SupportedSchemaVersion", value.Sel.Name)
			require.Equal(t, path, imports[origin.Name], "runtime/source schema version must come from its own backend package")
			found[key.Name]++
		}
		return true
	})
	require.Equal(t, map[string]int{"SupportedSchemaVersion": 1, "SourceSchemaVersion": 1}, found)
}
