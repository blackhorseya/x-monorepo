// Package archtest enforces the DDD rules depguard cannot see (depguard only
// knows imports). Each BC runs it from its own arch_test.go:
//
//	func TestArchitecture(t *testing.T) { archtest.Run(t, ".") }
//
// Rules are AST-level only (no type checking) so the package has zero
// dependencies and runs in milliseconds. Violations are reported with
// file:line so the agent / developer can fix them directly.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Rule inspects one parsed file and reports violations.
type Rule struct {
	Name  string
	Layer string // "domain" | "application/usecase" | "application/dto" | "application/port" | "adapters" | "" (all)
	Check func(fset *token.FileSet, f *ast.File, report func(pos token.Pos, msg string))
}

// Run applies DefaultRules to every non-test .go file under root.
func Run(t *testing.T, root string) {
	t.Helper()
	RunRules(t, root, DefaultRules()...)
}

// RunRules applies the given rules.
func RunRules(t *testing.T, root string, rules ...Rule) {
	t.Helper()
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "contract" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir // generated code
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".pb.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		layer := layerOf(filepath.ToSlash(path))
		currentFile = filepath.ToSlash(path)
		for _, r := range rules {
			if r.Layer != "" && !strings.HasPrefix(layer, r.Layer) {
				continue
			}
			r.Check(fset, f, func(pos token.Pos, msg string) {
				t.Errorf("[%s] %s: %s", r.Name, fset.Position(pos), msg)
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// isParamObject reports whether a domain struct is a constructor input (order.Params,
// order.ReconstituteParams): plain exported fields consumed once by New/Reconstitute,
// not aggregate state, so the private-field and constructor rules do not apply.
func isParamObject(name string) bool { return strings.HasSuffix(name, "Params") }

func layerOf(path string) string {
	for _, l := range []string{"application/usecase", "application/dto", "application/port", "application/mapper", "domain", "adapters", "infra"} {
		if strings.Contains("/"+path, "/"+l+"/") {
			return l
		}
	}
	return ""
}

// DefaultRules is the DDD rule set for this repo.
func DefaultRules() []Rule {
	return []Rule{
		{
			Name:  "domain-no-exported-fields",
			Layer: "domain",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forStructs(f, func(name string, st *ast.StructType) {
					if isParamObject(name) {
						return
					}
					for _, fld := range st.Fields.List {
						for _, n := range fld.Names {
							if n.IsExported() {
								report(n.Pos(), name+"."+n.Name+" is exported; domain state must be private (use a constructor + behaviour methods)")
							}
						}
					}
				})
			},
		},
		{
			Name:  "domain-no-setters",
			Layer: "domain",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forMethods(f, func(recv string, fn *ast.FuncDecl) {
					if strings.HasPrefix(fn.Name.Name, "Set") && len(fn.Name.Name) > 3 && ast.IsExported(fn.Name.Name) {
						report(fn.Pos(), recv+"."+fn.Name.Name+": name the behaviour (Cancel, Activate…), not the field being set")
					}
				})
			},
		},
		{
			Name:  "domain-has-constructor",
			Layer: "domain",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				// Every exported struct with private fields needs an exported constructor in the same file
				// (New, NewX, Reconstitute, or any func returning *X / X).
				ctors := map[string]bool{}
				for _, d := range f.Decls {
					fn, ok := d.(*ast.FuncDecl)
					if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil {
						continue
					}
					for _, r := range fn.Type.Results.List {
						if n := typeName(r.Type); n != "" {
							ctors[n] = true
						}
					}
				}
				forStructs(f, func(name string, st *ast.StructType) {
					if !ast.IsExported(name) || len(st.Fields.List) == 0 || ctors[name] || isParamObject(name) {
						return
					}
					report(st.Pos(), name+" has no constructor in this file; add New"+name+"()/Reconstitute() so it is never built in an invalid state")
				})
			},
		},
		{
			Name:  "domain-no-wall-clock",
			Layer: "domain",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forSelectors(f, func(sel *ast.SelectorExpr) {
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "time" && sel.Sel.Name == "Now" {
						report(sel.Pos(), "domain must not call time.Now(); take `now time.Time` as a parameter (port.Clock lives in application)")
					}
				})
			},
		},
		{
			Name:  "domain-no-logging",
			Layer: "domain",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				for _, imp := range f.Imports {
					p := strings.Trim(imp.Path.Value, `"`)
					if p == "log" || p == "log/slog" || p == "fmt" && usesPrint(f) {
						report(imp.Pos(), "domain must not log; return errors and let use cases / decorators log")
					}
				}
			},
		},
		{
			Name:  "repository-interface-in-domain",
			Layer: "",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forInterfaces(f, func(name string, _ *ast.InterfaceType, pos token.Pos) {
					if strings.HasSuffix(name, "Repository") && layerOf(filepath.ToSlash(fileName(f))) != "domain" {
						report(pos, name+": repository interfaces belong to the domain layer")
					}
				})
			},
		},
		{
			Name:  "usecase-shape",
			Layer: "application/usecase",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				// Every exported struct in usecase/ must have Execute(ctx, in) (out, error).
				methods := map[string]*ast.FuncDecl{}
				forMethods(f, func(recv string, fn *ast.FuncDecl) {
					if fn.Name.Name == "Execute" {
						methods[recv] = fn
					}
				})
				forStructs(f, func(name string, st *ast.StructType) {
					if !ast.IsExported(name) {
						return
					}
					fn, ok := methods[name]
					if !ok {
						report(st.Pos(), name+": use case must expose Execute(c context.Context, in I) (O, error)")
						return
					}
					if fn.Type.Params.NumFields() != 2 || fn.Type.Results.NumFields() != 2 {
						report(fn.Pos(), name+".Execute: must be Execute(c context.Context, in I) (O, error) so usecasex decorators can wrap it")
					}
				})
			},
		},
		{
			Name:  "usecase-single-responsibility",
			Layer: "application/usecase",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forMethods(f, func(recv string, fn *ast.FuncDecl) {
					if fn.Name.IsExported() && fn.Name.Name != "Execute" {
						report(fn.Pos(), recv+"."+fn.Name.Name+": a use case has exactly one public operation (Execute); split into another use case")
					}
				})
			},
		},
		{
			Name:  "dto-is-data-only",
			Layer: "application/dto",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forMethods(f, func(recv string, fn *ast.FuncDecl) {
					report(fn.Pos(), recv+"."+fn.Name.Name+": dto carries data only; put conversion in application/mapper")
				})
				for _, imp := range f.Imports {
					p := strings.Trim(imp.Path.Value, `"`)
					if strings.Contains(p, ".") && !strings.HasPrefix(p, "golang.org/x") { // non-stdlib
						report(imp.Pos(), "dto must not import "+p+"; it is a plain-Go boundary (no domain, no proto)")
					}
				}
			},
		},
		{
			Name:  "port-is-interfaces-only",
			Layer: "application/port",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forStructs(f, func(name string, st *ast.StructType) {
					report(st.Pos(), name+": port declares interfaces; implementations live in infra")
				})
				for _, d := range f.Decls {
					if fn, ok := d.(*ast.FuncDecl); ok {
						report(fn.Pos(), fn.Name.Name+": port declares interfaces only, no functions")
					}
				}
			},
		},
		{
			Name:  "adapter-depends-on-usecase-interface",
			Layer: "adapters",
			Check: func(_ *token.FileSet, f *ast.File, report func(token.Pos, string)) {
				forStructs(f, func(name string, st *ast.StructType) {
					for _, fld := range st.Fields.List {
						if se, ok := fld.Type.(*ast.StarExpr); ok {
							if sel, ok := se.X.(*ast.SelectorExpr); ok {
								if x, ok := sel.X.(*ast.Ident); ok && x.Name == "usecase" {
									report(fld.Pos(), name+": hold usecasex.UseCase[I, O], not *usecase."+sel.Sel.Name+", so decorators apply")
								}
							}
						}
					}
				})
			},
		},
	}
}

// ── AST helpers ──────────────────────────────────────────────────────────────

func forStructs(f *ast.File, fn func(name string, st *ast.StructType)) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, s := range gd.Specs {
			ts := s.(*ast.TypeSpec)
			if st, ok := ts.Type.(*ast.StructType); ok {
				fn(ts.Name.Name, st)
			}
		}
	}
}

func forInterfaces(f *ast.File, fn func(name string, it *ast.InterfaceType, pos token.Pos)) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, s := range gd.Specs {
			ts := s.(*ast.TypeSpec)
			if it, ok := ts.Type.(*ast.InterfaceType); ok {
				fn(ts.Name.Name, it, ts.Pos())
			}
		}
	}
}

func forMethods(f *ast.File, fn func(recv string, fd *ast.FuncDecl)) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		if n := typeName(fd.Recv.List[0].Type); n != "" {
			fn(n, fd)
		}
	}
}

func forSelectors(f *ast.File, fn func(*ast.SelectorExpr)) {
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			fn(sel)
		}
		return true
	})
}

func usesPrint(f *ast.File) bool {
	found := false
	forSelectors(f, func(sel *ast.SelectorExpr) {
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Print") {
			found = true
		}
	})
	return found
}

func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.IndexExpr: // generic instantiation
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	}
	return ""
}

func fileName(f *ast.File) string {
	// token.FileSet position is resolved by the caller; here we only need the path
	// which parser stored on the file's Pos via the fset. Walk back through the
	// comment map is overkill; the caller passes layer via Run, so for the
	// layer-less rule we re-derive from the first declaration's position file.
	return currentFile
}

// currentFile is set by Run before each rule execution (single-goroutine).
var currentFile string
