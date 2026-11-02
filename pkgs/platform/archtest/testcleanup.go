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
//   - pool-defer: a test function binds the closer returned by platformpg.NewDB
//     and defers it, and registers t.Cleanup anywhere in the same function. A
//     deferred call runs when the function returns, t.Cleanup after that, so a
//     cleanup that touches the pool runs on a closed pool and, when it discards
//     the error, nothing says so. THIS IS A SHAPE BAN, NOT A LIFECYCLE PROOF: it
//     does not ask which t the Cleanup is on, whether a closure got the parent's
//     t, or whether the Cleanup would in fact run before the close (a t.Run child's
//     does). Syntax cannot answer that for all shapes (two review rounds each
//     found another), and the fix is the same for every shape, so the gate bans
//     the pairing and the fix is always pgtest.DB.
//   - pool-order: the closer is registered with t.Cleanup AFTER another
//     t.Cleanup that reaches the pool. Cleanups run last-in first-out, so the
//     closer runs first and the earlier registration sees a closed pool.
//   - cleanup-swallow: a t.Cleanup body discards the error of a database write
//     (`_ = db.Exec(...).Error`, or a bare `db.Exec(...)` statement). A purge
//     that fails then leaves its fixture for the next run, which hides it by
//     wiping the table first.
//
// pgtest.DB registers the close itself and hands the caller no closer, so there is
// nothing to defer or to order.
//
// The scan is AST-only, like the rest of the package. "The closer" is found by
// binding, not by name: the second result of a NewDB call, whatever it is
// called (cleanup, dbCleanup, closeDB, ...).

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
	RuleDirectPool     = "test-direct-pool"
	RulePoolClose      = "test-pool-close"
)

// Allow exempts one repo-relative path prefix from the direct-pool rule, with the reason.
type Allow struct{ Path, Reason string }

// DirectPoolAllowlist is the whole list of exemptions; each line is one path and why.
// (Empty until the rule lands.)
var DirectPoolAllowlist = []Allow{}

// ScanTestCleanupDirAllow is ScanTestCleanupDir with an explicit allowlist (tests).
func ScanTestCleanupDirAllow(root string, allow []Allow) ([]Finding, ScanStats, error) {
	return ScanTestCleanupDir(root)
}

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

// ScanTestCleanupFiles applies the rules to the files of one package. The unit is
// a top-level function: its closures and t.Run bodies are part of it, not scopes
// of their own.
func ScanTestCleanupFiles(fset *token.FileSet, files []*ast.File) ([]Finding, ScanStats) {
	registrars := cleanupRegistrars(files)
	var (
		out   []Finding
		stats ScanStats
	)
	stats.Files = len(files)
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			stats.Funcs++
			found, st := scanFunc(fset, fn.Body, registrars)
			out = append(out, found...)
			stats.Closers += st.Closers
			stats.Defers += st.Defers
		}
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

// cleanupRegistrars names the package's functions that register a cleanup, directly
// (.Cleanup anywhere in the body) or by calling another such function
// (seedFixtures(t, db) registering a purge, a subtest body that calls it). Calling
// one, or handing one to something (t.Run("x", child)), counts as a t.Cleanup
// registration. By name only: which parameter it registers on is exactly the
// question the gate does not ask.
func cleanupRegistrars(files []*ast.File) map[string]bool {
	m := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil || m[fn.Name.Name] {
					continue
				}
				found := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && (isCleanupCall(c) || m[calleeName(c)]) {
						found = true
					}
					return !found
				})
				if found {
					m[fn.Name.Name] = true
					changed = true
				}
			}
		}
	}
	return m
}

func isCleanupCall(c *ast.CallExpr) bool {
	s, ok := c.Fun.(*ast.SelectorExpr)
	return ok && s.Sel.Name == "Cleanup" && len(c.Args) == 1
}

type closer struct {
	name string
	kind string // "NewDB" | "pool"
	db   string // the handle the pool serves: `db` in `db, cleanup, err := NewDB(...)`, the receiver of db.DB()
}

type reg struct {
	pos     token.Pos
	closes  string // closer name when this registration is the closer itself
	literal ast.Node
	uses    bool // reaches a pool handle: a literal naming it, or a registering helper given it; t.Cleanup(telCleanup) does not
}

// scanFunc applies the rules to one top-level function, closures included.
func scanFunc(fset *token.FileSet, body *ast.BlockStmt, registrars map[string]bool) ([]Finding, ScanStats) {
	var (
		out     []Finding
		stats   ScanStats
		closers = map[string]closer{}
		lits    = map[string]*ast.FuncLit{} // purge := func() { ... }
	)
	add := func(pos token.Pos, rule, msg string) {
		out = append(out, Finding{Pos: fset.Position(pos), Rule: rule, Msg: msg})
	}

	// Pass 1: bindings, anywhere in the function.
	ast.Inspect(body, func(n ast.Node) bool {
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
		return true
	})
	stats.Closers = len(closers)
	dbs := map[string]bool{}
	for _, c := range closers {
		if c.db != "" {
			dbs[c.db] = true
		}
	}

	// Pass 2: defers and Cleanup registrations, in source order, wherever they are.
	var (
		defers   []token.Pos
		registrs []reg
	)
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.DeferStmt:
			if closesPool(s.Call, closers) != "" {
				defers = append(defers, s.Pos())
			}
		case *ast.CallExpr:
			switch {
			case isCleanupCall(s):
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
				registrs = append(registrs, reg{pos: s.Pos(), uses: touches(s, dbs)})
			default:
				// A registering function handed over rather than called: t.Run("x", child).
				for _, a := range s.Args {
					if id, ok := a.(*ast.Ident); ok && registrars[id.Name] {
						registrs = append(registrs, reg{pos: a.Pos(), uses: true})
					}
				}
			}
		}
		return true
	})
	stats.Defers = len(defers)

	line := func(p token.Pos) int { return fset.Position(p).Line }

	// pool-defer: a shape ban. Not "this Cleanup runs after the close", just "both".
	if len(registrs) > 0 {
		for _, d := range defers {
			add(d, RulePoolDefer, "the pool is closed by a defer and this function also registers t.Cleanup (line "+
				strconv.Itoa(line(registrs[0].pos))+"); the pairing is banned, whether or not that Cleanup would run before the close: use pgtest.DB, which registers the close with t.Cleanup so the order is always right")
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
