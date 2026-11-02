package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the gate behind pgtest.DB (#371). It scans *_test.go, which
// RunRules deliberately skips, for the three ways a Postgres test leaves rows
// behind while still reporting PASS:
//
//   - pool-defer: a function binds the closer returned by platformpg.NewDB and
//     defers it, and the same function also registers t.Cleanup. A deferred
//     call runs when the function returns, t.Cleanup runs after that, so the
//     cleanup talks to a closed pool and (when it discards the error) nothing
//     says so.
//   - pool-order: the closer is registered with t.Cleanup AFTER another
//     t.Cleanup. Cleanups run last-in first-out, so the closer runs first and
//     the earlier registration sees a closed pool all the same.
//   - cleanup-swallow: a t.Cleanup body discards the error of a database write
//     (`_ = db.Exec(...).Error`, or a bare `db.Exec(...)` statement). A purge
//     that fails then leaves its fixture for the next run, which hides it by
//     wiping the table first.
//
// The fix for all three is the same: pgtest.DB registers the close itself and
// hands the caller no closer, so there is nothing to defer or to order.
//
// The scan is AST-only, like the rest of the package. "The closer" is found by
// binding, not by name: the second result of a NewDB call, whatever it is
// called (cleanup, dbCleanup, closeDB, ...). That is a different set from a
// grep for `defer cleanup()`.

// Finding is one violation, positioned at the statement to fix.
type Finding struct {
	Pos  token.Position
	Rule string
	Msg  string
}

func (f Finding) String() string { return "[" + f.Rule + "] " + f.Pos.String() + ": " + f.Msg }

// Rule names reported by ScanTestCleanup.
const (
	RulePoolDefer      = "test-cleanup-pool-defer"
	RulePoolOrder      = "test-cleanup-pool-order"
	RuleCleanupSwallow = "test-cleanup-swallow"
)

// ScanStats says how much a scan looked at. A gate that scanned nothing is
// green for the wrong reason, so callers assert on it.
type ScanStats struct {
	Files   int // *_test.go files parsed
	Funcs   int // function bodies (decls and literals) analysed
	Closers int // NewDB closers (and pool Close handles) bound
	Defers  int // of which deferred
}

func (s *ScanStats) add(o ScanStats) {
	s.Files += o.Files
	s.Funcs += o.Funcs
	s.Closers += o.Closers
	s.Defers += o.Defers
}

// RunTestCleanup scans every *_test.go under root (a module directory) and
// fails the test on each finding. It returns the stats so the caller can pin
// that something was scanned.
func RunTestCleanup(t *testing.T, root string) ScanStats {
	t.Helper()
	findings, stats, err := ScanTestCleanupDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f.String())
	}
	if stats.Files == 0 {
		t.Fatalf("test-cleanup gate scanned no *_test.go under %s: a gate over an empty set is green for nothing", root)
	}
	return stats
}

// ScanTestCleanupDir parses every *_test.go under root and applies the rules.
// Files are grouped by directory so that a helper declared in one file and
// called from another (same package) is seen.
func ScanTestCleanupDir(root string) ([]Finding, ScanStats, error) {
	fset := token.NewFileSet()
	byDir := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		dir := filepath.Dir(path)
		byDir[dir] = append(byDir[dir], f)
		return nil
	})
	if err != nil {
		return nil, ScanStats{}, err
	}

	var (
		all   []Finding
		stats ScanStats
	)
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		found, st := ScanTestCleanupFiles(fset, byDir[d])
		all = append(all, found...)
		stats.add(st)
	}
	return all, stats, nil
}

// ScanTestCleanupFiles applies the rules to the files of one package.
func ScanTestCleanupFiles(fset *token.FileSet, files []*ast.File) ([]Finding, ScanStats) {
	registrars := cleanupRegistrars(files)
	var (
		out   []Finding
		stats ScanStats
	)
	stats.Files = len(files)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			var body *ast.BlockStmt
			switch fn := n.(type) {
			case *ast.FuncDecl:
				body = fn.Body
			case *ast.FuncLit:
				body = fn.Body
			}
			if body == nil {
				return true
			}
			stats.Funcs++
			found, st := scanScope(fset, body, registrars)
			out = append(out, found...)
			stats.Closers += st.Closers
			stats.Defers += st.Defers
			return true
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Pos, out[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
	return out, stats
}

// cleanupRegistrars names the package's functions that call .Cleanup on
// something themselves (seedFixtures(t, db) registering a purge): calling one
// counts as a t.Cleanup registration in the caller's scope.
func cleanupRegistrars(files []*ast.File) map[string]bool {
	m := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && isCleanupCall(c) {
					found = true
				}
				return !found
			})
			if found {
				m[fn.Name.Name] = true
			}
		}
	}
	return m
}

func isCleanupCall(c *ast.CallExpr) bool {
	s, ok := c.Fun.(*ast.SelectorExpr)
	return ok && s.Sel.Name == "Cleanup" && len(c.Args) == 1
}

// ownStmts walks body without descending into nested function literals: each
// literal is its own scope (a subtest's t.Cleanup runs when the subtest ends,
// before the parent's defers).
func ownStmts(body *ast.BlockStmt, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if n != nil {
			visit(n)
		}
		return true
	})
}

type closer struct {
	name string
	kind string // "NewDB" | "pool"
	db   string // the handle the pool serves: `db` in `db, cleanup, err := NewDB(...)`, the receiver of db.DB()
}

func scanScope(fset *token.FileSet, body *ast.BlockStmt, registrars map[string]bool) ([]Finding, ScanStats) {
	var (
		out     []Finding
		stats   ScanStats
		closers = map[string]closer{}
		lits    = map[string]*ast.FuncLit{} // purge := func() { ... }
	)
	add := func(pos token.Pos, rule, msg string) {
		out = append(out, Finding{Pos: fset.Position(pos), Rule: rule, Msg: msg})
	}

	// Pass 1: bindings.
	ownStmts(body, func(n ast.Node) {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) == 1 {
				if lit, ok := s.Rhs[0].(*ast.FuncLit); ok && len(s.Lhs) == 1 {
					if id, ok := s.Lhs[0].(*ast.Ident); ok {
						lits[id.Name] = lit
					}
				}
			}
			bindClosers(s.Lhs, s.Rhs, closers)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(s.Names))
			for i, n := range s.Names {
				lhs[i] = n
			}
			bindClosers(lhs, s.Values, closers)
			if len(s.Names) == 1 && len(s.Values) == 1 {
				if lit, ok := s.Values[0].(*ast.FuncLit); ok {
					lits[s.Names[0].Name] = lit
				}
			}
		}
	})
	stats.Closers = len(closers)
	dbs := map[string]bool{}
	for _, c := range closers {
		if c.db != "" {
			dbs[c.db] = true
		}
	}

	// Pass 2: defers, Cleanup registrations, in source order. A defer counts only in
	// this function's own body (a literal's defer runs when the literal returns).
	// A Cleanup counts when it is registered on this function's t: a closure that
	// captures t (open := func() { t.Cleanup(...) }) registers on the same t, so
	// it runs after this function's defers; the literal given to t.Run is a subtest
	// with a t of its own, which finishes first. A closure that merely takes a t
	// parameter is not one: func(t *testing.T) {...}(t) is the parent's t.
	type reg struct {
		pos     token.Pos
		closes  string // closer name when this registration is the closer itself
		literal ast.Node
		uses    bool // reaches a pool handle: a literal naming it, or a registering helper given it; t.Cleanup(telCleanup) does not
	}
	var (
		defers   []token.Pos
		registrs []reg
	)
	var walk func(n ast.Node, own bool, shadow map[string]bool)
	visit := func(n ast.Node, own bool, shadow map[string]bool) {
		switch s := n.(type) {
		case *ast.DeferStmt:
			if own && closesPool(s.Call, closers) != "" {
				defers = append(defers, s.Pos())
			}
		case *ast.CallExpr:
			switch {
			case isCleanupCall(s):
				if id, ok := s.Fun.(*ast.SelectorExpr).X.(*ast.Ident); ok && shadow[id.Name] {
					return // a subtest's (or another closure's) own t
				}
				r := reg{pos: s.Pos()}
				switch a := s.Args[0].(type) {
				case *ast.Ident:
					if _, ok := closers[a.Name]; ok {
						r.closes = a.Name
					} else if lit, ok := lits[a.Name]; ok {
						r.literal, r.uses = lit, touches(lit, dbs)
					}
				case *ast.SelectorExpr: // t.Cleanup(sqlDB.Close)
					if id, ok := a.X.(*ast.Ident); ok && a.Sel.Name == "Close" {
						if c, ok := closers[id.Name]; ok && c.kind == "pool" {
							r.closes = id.Name
						}
					}
				case *ast.FuncLit:
					r.literal, r.uses = a, touches(a, dbs)
					if name := closesPool(&ast.CallExpr{Fun: a}, closers); name != "" {
						r.closes = name
					}
				}
				registrs = append(registrs, r)
			case calleeName(s) != "" && registrars[calleeName(s)]:
				// A same-package helper that registers its own cleanup, on the t it is
				// given: a subtest's t finishes before this function's defers run.
				for _, a := range s.Args {
					if id, ok := a.(*ast.Ident); ok && shadow[id.Name] {
						return
					}
				}
				registrs = append(registrs, reg{pos: s.Pos(), uses: touches(s, dbs)})
			}
		}
	}
	subtests := map[*ast.FuncLit]bool{} // the literals handed to X.Run(name, func(t *testing.T) {...})
	walk = func(root ast.Node, own bool, shadow map[string]bool) {
		ast.Inspect(root, func(n ast.Node) bool {
			if n == nil || n == root {
				return n == root
			}
			if c, ok := n.(*ast.CallExpr); ok && calleeName(c) == "Run" {
				for _, a := range c.Args {
					if lit, ok := a.(*ast.FuncLit); ok {
						subtests[lit] = true
					}
				}
			}
			if lit, ok := n.(*ast.FuncLit); ok {
				inner := map[string]bool{}
				for k := range shadow {
					inner[k] = true
				}
				// Only a subtest gets a t of its own. Any other literal that takes a *testing.T
				// (called at once with the parent's t, passed on, run in a goroutine) is handed
				// the same t, so its Cleanup still runs after this function's defers.
				if subtests[lit] {
					for _, f := range lit.Type.Params.List {
						for _, nm := range f.Names {
							inner[nm.Name] = true
						}
					}
				}
				walk(lit.Body, false, inner)
				return false
			}
			visit(n, own, shadow)
			return true
		})
	}
	walk(body, true, map[string]bool{})
	stats.Defers = len(defers)

	line := func(p token.Pos) int { return fset.Position(p).Line }

	// pool-defer: any other t.Cleanup counts, a literal or a bare function alike
	if len(registrs) > 0 {
		for _, d := range defers {
			add(d, RulePoolDefer, "the pool is closed by a defer, but this function also registers t.Cleanup (line "+
				strconv.Itoa(line(registrs[0].pos))+"), which runs after the defer on a closed pool; use pgtest.DB (it registers the close with t.Cleanup, so a later t.Cleanup runs first)")
		}
	}
	// pool-order
	for i, r := range registrs {
		if r.closes == "" {
			continue
		}
		for _, earlier := range registrs[:i] {
			if earlier.closes == "" && earlier.uses {
				add(r.pos, RulePoolOrder, "the pool is closed by t.Cleanup registered after the t.Cleanup on line "+
					strconv.Itoa(line(earlier.pos))+"; cleanups run last-in first-out, so the close runs first; use pgtest.DB")
				break
			}
		}
	}
	// cleanup-swallow
	for _, r := range registrs {
		if r.literal == nil {
			continue
		}
		lit, ok := r.literal.(*ast.FuncLit)
		if !ok {
			continue
		}
		ast.Inspect(lit.Body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				if allBlank(s.Lhs) && len(s.Rhs) == 1 && dbWrite(s.Rhs[0]) {
					add(s.Pos(), RuleCleanupSwallow, "t.Cleanup discards the error of a database write; check it (t.Errorf), a failed purge leaves its rows behind")
				}
			case *ast.ExprStmt:
				if dbWrite(s.X) {
					add(s.Pos(), RuleCleanupSwallow, "t.Cleanup drops the result of a database write; check its error, a failed purge leaves its rows behind")
				}
			}
			return true
		})
	}
	return out, stats
}

// bindClosers records the closer a NewDB-shaped call returns. `db, cl, err :=
// X.NewDB(...)` binds cl; `sqlDB, err := db.DB()` binds sqlDB (its Close is the
// pool's).
func bindClosers(lhs, rhs []ast.Expr, into map[string]closer) {
	if len(rhs) != 1 {
		return
	}
	c, ok := rhs[0].(*ast.CallExpr)
	if !ok {
		return
	}
	switch name := calleeName(c); {
	case name == "NewDB" && len(lhs) == 3:
		if id, ok := lhs[1].(*ast.Ident); ok && id.Name != "_" {
			db := ""
			if h, ok := lhs[0].(*ast.Ident); ok {
				db = h.Name
			}
			into[id.Name] = closer{name: id.Name, kind: "NewDB", db: db}
		}
	case name == "DB" && len(c.Args) == 0 && len(lhs) == 2:
		if id, ok := lhs[0].(*ast.Ident); ok && id.Name != "_" {
			db := ""
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				if h, ok := sel.X.(*ast.Ident); ok {
					db = h.Name
				}
			}
			into[id.Name] = closer{name: id.Name, kind: "pool", db: db}
		}
	}
}

// closesPool: does the call invoke a bound closer (cleanup(), sqlDB.Close(), or
// a func literal calling one)? It returns the closer's name.
func closesPool(c *ast.CallExpr, closers map[string]closer) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		if cl, ok := closers[f.Name]; ok && cl.kind == "NewDB" {
			return f.Name
		}
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok && f.Sel.Name == "Close" {
			if cl, ok := closers[id.Name]; ok && cl.kind == "pool" {
				return id.Name
			}
		}
	case *ast.FuncLit:
		name := ""
		ast.Inspect(f.Body, func(n ast.Node) bool {
			if ic, ok := n.(*ast.CallExpr); ok && name == "" {
				name = closesPool(ic, closers)
			}
			return name == ""
		})
		return name
	}
	return ""
}

// touches: does the node mention one of the pool handles?
func touches(n ast.Node, dbs map[string]bool) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && dbs[id.Name] {
			found = true
		}
		return !found
	})
	return found
}

func calleeName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func allBlank(lhs []ast.Expr) bool {
	if len(lhs) == 0 {
		return false
	}
	for _, e := range lhs {
		if id, ok := e.(*ast.Ident); !ok || id.Name != "_" {
			return false
		}
	}
	return true
}

// writeMethods are the gorm / database/sql calls that change rows.
var writeMethods = map[string]bool{
	"Exec": true, "ExecContext": true, "Delete": true, "Create": true,
	"Save": true, "Update": true, "Updates": true, "Truncate": true,
}

// dbWrite: is the expression itself a database write, or the .Error read off
// one? Only the statement's own chain counts: check(db.Exec(..).Error) hands the
// error to check, which is not a swallow.
func dbWrite(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CallExpr:
		return writeMethods[calleeName(x)]
	case *ast.SelectorExpr:
		return x.Sel.Name == "Error" && dbWrite(x.X)
	}
	return false
}
