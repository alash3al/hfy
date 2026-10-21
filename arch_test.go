package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The rules in README "Conventions" that a reviewer could miss are checked
// here, on the source, so they hold however many people contribute.

const module = "github.com/alash3al/hfy"

// layers is the architecture: the internal packages each package may import.
// Changing it is a design decision, made here on purpose, never by accident.
var layers = map[string][]string{
	"":                           {"agent", "config", "db", "httpapi", "jobs", "mcp", "metrics", "types"},
	"internal/types":             {}, // shared vocabulary: depends on nothing
	"internal/config":            {},
	"internal/metrics":           {},
	"internal/db":                {"types"},
	"internal/db/dbtest":         {"db"},
	"internal/jobs":              {"db", "metrics", "types"},
	"internal/provider":          {"metrics", "types"},
	"internal/provider/fakeprov": {},
	"internal/mcp":               {"config"},
	"internal/mcp/builtins":      {"config", "mcp"},
	"internal/mcp/fakemcp":       {},
	"internal/svc":               {"config", "db", "metrics", "types"}, // owns turn state; never the agent
	"internal/agent":             {"config", "db", "jobs", "mcp", "mcp/builtins", "metrics", "provider", "svc", "types"},
	"internal/httpapi":           {"config", "db", "metrics", "svc", "types"}, // never the agent: API and workers scale apart
}

type goFile struct {
	pkg string // directory relative to the module root
	ast *ast.File
}

var fset = token.NewFileSet()

func sources(t *testing.T) []goFile {
	t.Helper()
	var out []goFile
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(filepath.Dir(path))
		if pkg == "." {
			pkg = ""
		}
		out = append(out, goFile{pkg: pkg, ast: f})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLayering(t *testing.T) {
	for _, f := range sources(t) {
		allowed, known := layers[f.pkg]
		if !known {
			t.Errorf("%s: package %q is not in the layers table (arch_test.go); add it with its allowed imports", pos(f.ast), f.pkg)
			continue
		}
		for _, imp := range f.ast.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p != module && !strings.HasPrefix(p, module+"/") {
				continue
			}
			dep := strings.TrimPrefix(strings.TrimPrefix(p, module), "/")
			dep = strings.TrimPrefix(dep, "internal/")
			if !slices.Contains(allowed, dep) {
				t.Errorf("%s: %q may not import %q (allowed: %v)", pos(imp), f.pkg, dep, allowed)
			}
		}
	}
}

// Inside a transaction every read and write goes through it: a pool
// connection taken while the transaction holds one deadlocks once the pool is
// exhausted. Any function that receives a pgx.Tx must not touch .Pool.
func TestTransactionsUseOnlyTheirConnection(t *testing.T) {
	for _, f := range sources(t) {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			var typ *ast.FuncType
			var body *ast.BlockStmt
			switch fn := n.(type) {
			case *ast.FuncDecl:
				typ, body = fn.Type, fn.Body
			case *ast.FuncLit:
				typ, body = fn.Type, fn.Body
			default:
				return true
			}
			if body == nil || !takesTx(typ) {
				return true
			}
			ast.Inspect(body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Pool" {
					t.Errorf("%s: uses .Pool inside a function that holds a pgx.Tx (use the tx)", pos(sel))
				}
				return true
			})
			return false // checked; nested literals are inside the same transaction
		})
	}
}

func takesTx(ft *ast.FuncType) bool {
	for _, p := range ft.Params.List {
		if sel, ok := p.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Tx" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "pgx" {
				return true
			}
		}
	}
	return false
}

// svc owns turn state: only it appends turn.state events, under the conversation
// lock. Everyone else asks svc (StartTurn, EndTurn, RequestApproval, Stop).
func TestOnlySvcWritesTurnState(t *testing.T) {
	appends := []string{"AppendEvent", "append", "appendRow", "appendTo"}
	for _, f := range sources(t) {
		if f.pkg == "internal/svc" {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !slices.Contains(appends, sel.Sel.Name) {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(n ast.Node) bool {
					if lit, ok := n.(*ast.CompositeLit); ok {
						if s, ok := lit.Type.(*ast.SelectorExpr); ok && s.Sel.Name == "TurnStateEvent" {
							t.Errorf("%s: appends a turn.state event outside svc", pos(lit))
						}
					}
					return true
				})
			}
			return true
		})
	}
}

func pos(n ast.Node) string {
	p := fset.Position(n.Pos())
	return p.Filename + ":" + strconv.Itoa(p.Line)
}
