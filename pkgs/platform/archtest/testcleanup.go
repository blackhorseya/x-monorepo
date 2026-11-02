package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This file is the gate behind pgtest.DB (#371). It scans *_test.go, which
// RunRules deliberately skips, for the ways a Postgres test leaves rows behind
// while still reporting PASS: closing the pool before a t.Cleanup purge runs
// (a defer, a close registered after the purge, a close hidden behind an alias or
// a wrapper) and a purge that discards its error.
//
// The rule is: A TEST DOES NOT OPEN A POOL ITSELF. Any mention of a function that
// opens one (NewDB, sql.Open, pgxpool.New..., pgx.Connect..., gorm.Open) in a
// *_test.go file is a finding, whether it is called, assigned, passed on, kept in
// a struct or imported under another name. It is matched on identifiers and
// selectors alone, with no data flow. With no pool opened by the test there is no
// closer to defer, alias, wrap or register in the wrong order, so none of those
// shapes has to be understood. Earlier versions paired "defer the close" with
// "registers a t.Cleanup" and then had to decide which testing.T a Cleanup was on
// and follow aliases; three review rounds each found another shape that slipped
// through, so the pairing rule was removed rather than extended.
//
// The fix is always the same: pgtest.DB (pool closed by t.Cleanup, no closer
// handed back), pgtest.TryDB (the same when the test expects the open to fail) or
// pgtest.Conn (one pgx connection). Only the pgtest package itself may open a
// pool. A test that really must open one (for instance, a test of NewDB's own
// contract that TryDB cannot express) is listed in DirectPoolAllowlist: one
// path and one reason per line, in this file, not a comment that skips a check.
//
// Two rules remain next to it:
//
//   - pool-close: Close called on the *sql.DB behind a pgtest.DB (the value of
//     db.DB()). gorm.DB has no Close, so this is the one way left for a test to
//     close a pgtest.DB pool itself. One hop of binding (sqlDB, _ := db.DB();
//     sqlDB.Close()); an alias of sqlDB is not followed, and review must catch it.
//   - cleanup-swallow: a t.Cleanup body discards the error of a database write
//     (`_ = db.Exec(...).Error`, or a bare `db.Exec(...)` statement). A purge that
//     fails then leaves its fixture for the next run, which hides it by wiping the
//     table first.

// Finding is one violation, positioned at the statement to fix.
type Finding struct {
	Pos  token.Position
	Rule string
	Msg  string
}

func (f Finding) String() string { return "[" + f.Rule + "] " + f.Pos.String() + ": " + f.Msg }

// Rule names reported by ScanTestCleanupFiles.
const (
	RuleDirectPool     = "test-direct-pool"
	RulePoolClose      = "test-pool-close"
	RuleCleanupSwallow = "test-cleanup-swallow"
)

// Allow exempts one repo-relative path prefix from the direct-pool rule, with the
// reason it must open a pool itself.
type Allow struct{ Path, Reason string }

// DirectPoolAllowlist is the whole list of exemptions; each entry is one path and
// why. Keep it short: every entry is a test the helpers could not express.
var DirectPoolAllowlist = []Allow{}

// ScanStats says how much a scan looked at. A gate that scanned nothing is green
// for the wrong reason, so callers assert on it.
type ScanStats struct {
	Files int // *_test.go files parsed
	Funcs int // function declarations analysed
}

func (s *ScanStats) add(o ScanStats) {
	s.Files += o.Files
	s.Funcs += o.Funcs
}

// RunTestCleanup scans every *_test.go under root (a module directory) with the
// repo's DirectPoolAllowlist and fails the test on each finding. It returns the
// stats so the caller can pin that something was scanned.
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

// ScanTestCleanupDir parses every *_test.go under root and applies the rules with
// the repo's allowlist.
func ScanTestCleanupDir(root string) ([]Finding, ScanStats, error) {
	return ScanTestCleanupDirAllow(root, DirectPoolAllowlist)
}

// ScanTestCleanupDirAllow is ScanTestCleanupDir with an explicit allowlist. Paths
// in the allowlist are relative to the repository root (the directory holding
// go.work above root), or to root itself when there is none.
func ScanTestCleanupDirAllow(root string, allow []Allow) ([]Finding, ScanStats, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, ScanStats{}, err
	}
	base := repoRoot(abs)

	fset := token.NewFileSet()
	var (
		all   []Finding
		stats ScanStats
	)
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != abs && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata") {
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
		rel, rerr := filepath.Rel(base, path)
		if rerr != nil {
			return rerr
		}
		found, st := scanFile(fset, f, exemptFromDirectPool(filepath.ToSlash(rel), allow))
		all = append(all, found...)
		stats.add(st)
		return nil
	})
	if err != nil {
		return nil, ScanStats{}, err
	}
	sortFindings(all)
	return all, stats, nil
}

// ScanTestCleanupFiles applies the rules to already parsed files, none of them
// exempt (fixtures).
func ScanTestCleanupFiles(fset *token.FileSet, files []*ast.File) ([]Finding, ScanStats) {
	var (
		out   []Finding
		stats ScanStats
	)
	for _, f := range files {
		found, st := scanFile(fset, f, false)
		out = append(out, found...)
		stats.add(st)
	}
	sortFindings(out)
	return out, stats
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i].Pos, fs[j].Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Offset < b.Offset
	})
}

// repoRoot is the directory holding go.work at or above dir, or dir itself.
func repoRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// exemptFromDirectPool: the pgtest package opens the pools, so it is exempt by
// construction (any directory named pgtest); everything else needs an allowlist
// line.
func exemptFromDirectPool(rel string, allow []Allow) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == "pgtest" {
			return true
		}
	}
	for _, a := range allow {
		if strings.HasPrefix(rel, a.Path) {
			return true
		}
	}
	return false
}

// openerSelectors are the package.Name pairs that open a pool or a connection;
// "*" is every name of that package. NewDB is matched on its name alone, under any
// import name (or none).
var openerSelectors = map[string]map[string]bool{
	"sql":     {"Open": true, "OpenDB": true},
	"pgxpool": {"*": true},
	"pgx":     {"Connect": true, "ConnectConfig": true},
	"gorm":    {"Open": true},
	"stdlib":  {"OpenDB": true, "OpenDBFromPool": true},
	"pgconn":  {"Connect": true, "ConnectConfig": true},
}

const (
	newDB         = "NewDB"
	directPoolMsg = "a test opens a pool itself (%s); use pgtest.DB (pool closed by t.Cleanup), pgtest.TryDB (when the open is expected to fail) or pgtest.Conn (one connection). The shape is banned whatever is done with the closer afterwards: it is not a claim that this test is broken"
)

func isOpenerSelector(s *ast.SelectorExpr) bool {
	if s.Sel.Name == newDB {
		return true
	}
	x, ok := s.X.(*ast.Ident)
	if !ok {
		return false
	}
	names := openerSelectors[x.Name]

	return names != nil && (names["*"] || names[s.Sel.Name])
}

func selName(s *ast.SelectorExpr) string {
	if x, ok := s.X.(*ast.Ident); ok {
		return x.Name + "." + s.Sel.Name
	}

	return s.Sel.Name
}

func scanFile(fset *token.FileSet, f *ast.File, exempt bool) ([]Finding, ScanStats) {
	var (
		out   []Finding
		stats = ScanStats{Files: 1}
	)

	// direct-pool: identifiers and selectors, nothing else. A selector's Sel and a
	// declaration's name are not separate mentions.
	if !exempt {
		skip := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				skip[x.Name] = true
			case *ast.SelectorExpr:
				skip[x.Sel] = true
				if isOpenerSelector(x) {
					out = append(out, Finding{Pos: fset.Position(x.Pos()), Rule: RuleDirectPool, Msg: strings.Replace(directPoolMsg, "%s", selName(x), 1)})
				}
			case *ast.Ident:
				if x.Name == newDB && !skip[x] {
					out = append(out, Finding{Pos: fset.Position(x.Pos()), Rule: RuleDirectPool, Msg: strings.Replace(directPoolMsg, "%s", newDB, 1)})
				}
			}

			return true
		})
	}

	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		stats.Funcs++
		out = append(out, scanFunc(fset, fn.Body)...)
	}

	return out, stats
}

func isCleanupCall(c *ast.CallExpr) bool {
	s, ok := c.Fun.(*ast.SelectorExpr)

	return ok && s.Sel.Name == "Cleanup" && len(c.Args) == 1
}

// scanFunc applies pool-close and cleanup-swallow to one top-level function,
// closures included.
func scanFunc(fset *token.FileSet, body *ast.BlockStmt) []Finding {
	var out []Finding
	add := func(pos token.Pos, rule, msg string) {
		out = append(out, Finding{Pos: fset.Position(pos), Rule: rule, Msg: msg})
	}

	sqlDBs := map[string]bool{}       // sqlDB in `sqlDB, err := db.DB()`
	lits := map[string]*ast.FuncLit{} // purge := func() { ... }
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) != 1 {
				return true
			}
			if lit, ok := s.Rhs[0].(*ast.FuncLit); ok && len(s.Lhs) == 1 {
				if id, ok := s.Lhs[0].(*ast.Ident); ok {
					lits[id.Name] = lit
				}
			}
			if c, ok := s.Rhs[0].(*ast.CallExpr); ok && len(c.Args) == 0 && len(s.Lhs) == 2 {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DB" {
					if id, ok := s.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
						sqlDBs[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			if len(s.Names) == 1 && len(s.Values) == 1 {
				if lit, ok := s.Values[0].(*ast.FuncLit); ok {
					lits[s.Names[0].Name] = lit
				}
			}
		}

		return true
	})

	ast.Inspect(body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// pool-close
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" && len(c.Args) == 0 {
			if id, ok := sel.X.(*ast.Ident); ok && sqlDBs[id.Name] {
				add(c.Pos(), RulePoolClose, "Close on the sql.DB behind a pgtest.DB; pgtest closes the pool, a test that closes it itself runs its own cleanups on a closed pool")
			}
		}
		// cleanup-swallow
		if !isCleanupCall(c) {
			return true
		}
		var lit *ast.FuncLit
		switch a := c.Args[0].(type) {
		case *ast.FuncLit:
			lit = a
		case *ast.Ident:
			lit = lits[a.Name]
		}
		if lit == nil {
			return true
		}
		ast.Inspect(lit.Body, func(m ast.Node) bool {
			switch s := m.(type) {
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

		return true
	})

	return out
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
