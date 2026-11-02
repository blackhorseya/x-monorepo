package archtest_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/archtest"
)

// scanSrc runs the gate on one in-memory file. The source is only parsed, never
// compiled, so a fixture can be as short as the shape it shows.
func scanSrc(t *testing.T, src string) ([]archtest.Finding, archtest.ScanStats) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture_test.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return archtest.ScanTestCleanupFiles(fset, []*ast.File{f})
}

// hits renders findings as "rule@line", the line being the fixture line the
// finding must point at (counted from the first line of the literal).
func hits(fs []archtest.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s@%03d", f.Rule, f.Pos.Line))
	}
	sort.Strings(out)
	return out
}

// The negative cases are the point: each one must be observed to hit (a count
// above zero, at the exact line), or a scanner that matched nothing would be
// green forever.
func TestTestCleanup_Fixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{
			// The shape of #366 before its fix (and #342, #318): the pool is closed by a
			// defer, so the Cleanup registered later runs on a closed pool, and swallows
			// the error that says so.
			name: "defer then cleanup (#366 before the fix)",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	defer cleanup()
	t.Cleanup(func() { _ = db.Exec("DELETE FROM t").Error })
}`,
			want: []string{"test-cleanup-pool-defer@004", "test-cleanup-swallow@005"},
		},
		{
			// The closer is found by what NewDB returns, not by the name "cleanup".
			name: "renamed closers",
			src: `package x_test
func TestX(t *testing.T) {
	a, dbCleanup, _ := postgres.NewDB(cfg, tel)
	defer dbCleanup()
	b, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Cleanup(func() { if err := a.Exec("x").Error; err != nil { t.Error(err) } })
	_ = b
}`,
			want: []string{"test-cleanup-pool-defer@004", "test-cleanup-pool-defer@006"},
		},
		{
			name: "pool closed through sql.DB",
			src: `package x_test
func TestX(t *testing.T) {
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	t.Cleanup(func() { check(db.Exec("x").Error) })
}`,
			want: []string{"test-cleanup-pool-defer@004"},
		},
		{
			name: "deferred func literal that closes the pool",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer func() { cleanup() }()
	t.Cleanup(func() { check(db.Exec("x").Error) })
}`,
			want: []string{"test-cleanup-pool-defer@004"},
		},
		{
			// LIFO: the close registered after the purge runs before it.
			name: "close registered after the purge",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	t.Cleanup(func() { check(db.Exec("x").Error) })
	t.Cleanup(cleanup)
}`,
			want: []string{"test-cleanup-pool-order@005"},
		},
		{
			name: "purge kept in a variable, close registered after it",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	purge := func() { check(db.Exec("x").Error) }
	t.Cleanup(purge)
	t.Cleanup(cleanup)
}`,
			want: []string{"test-cleanup-pool-order@006"},
		},
		{
			// A same-package helper registers the purge: the caller's defer is still wrong.
			name: "helper that registers t.Cleanup",
			src: `package x_test
func seed(t *testing.T, db *gorm.DB) { t.Cleanup(func() { check(db.Exec("x").Error) }) }
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer cleanup()
	seed(t, db)
}`,
			want: []string{"test-cleanup-pool-defer@005"},
		},
		{
			// A closure that captures t registers on the same t: its Cleanup runs after
			// the function's defers (eventbus/start_test.go before #371).
			name: "Cleanup registered by a closure that captures t",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer cleanup()
	open := func() {
		sub := subscribe(db)
		t.Cleanup(func() { _ = sub.Close() })
	}
	open()
}`,
			want: []string{"test-cleanup-pool-defer@004"},
		},
		{
			// Named t, same pointer: an immediately called closure is not a subtest.
			name: "immediately called closure given the parent's t",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer cleanup()
	func(t *testing.T) {
		t.Cleanup(func() { check(db.Exec("x").Error) })
	}(t)
}`,
			want: []string{"test-cleanup-pool-defer@004"},
		},
		{
			name: "helper given the parent's t from a plain closure",
			src: `package x_test
func registerPurge(t *testing.T, db *gorm.DB) { t.Cleanup(func() { check(db.Exec("x").Error) }) }
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer cleanup()
	run := func(t *testing.T) { registerPurge(t, db) }
	run(t)
}`,
			want: []string{"test-cleanup-pool-defer@005"},
		},
		{
			name: "swallowed write: blank assignment, bare call, bare .Error, discarded pgx result",
			src: `package x_test
func TestX(t *testing.T) {
	t.Cleanup(func() { _ = db.Exec("a").Error })
	t.Cleanup(func() { db.Exec("b") })
	t.Cleanup(func() { db.Exec("c").Error })
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "d") })
}`,
			want: []string{"test-cleanup-swallow@003", "test-cleanup-swallow@004", "test-cleanup-swallow@005", "test-cleanup-swallow@006"},
		},
		{
			name: "swallowed write in a purge variable",
			src: `package x_test
func TestX(t *testing.T) {
	purge := func() {
		_ = db.Exec("a").Error
	}
	t.Cleanup(purge)
}`,
			want: []string{"test-cleanup-swallow@004"},
		},

		// ── what must stay green ────────────────────────────────────────────
		{
			name: "pgtest.DB: no closer in the caller, checked purge",
			src: `package x_test
func TestX(t *testing.T) {
	db := pgtest.DB(t, cfg, tel)
	t.Cleanup(func() {
		if err := db.Exec("x").Error; err != nil {
			t.Errorf("purge: %v", err)
		}
	})
}`,
		},
		{
			name: "close registered first, purge after (LIFO runs the purge first)",
			src: `package x_test
func TestX(t *testing.T) {
	_, telCleanup, _ := otel.New(a, b)
	t.Cleanup(telCleanup)
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	t.Cleanup(cleanup)
	t.Cleanup(func() { check(db.Exec("x").Error) })
}`,
		},
		{
			name: "defer alone is only style",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer cleanup()
	if err := db.Exec("x").Error; err != nil { t.Fatal(err) }
}`,
		},
		{
			// Forbidden shape, not a lifecycle claim: a subtest's Cleanup does run before the
			// parent's defers, but the gate does not decide which t a Cleanup belongs to.
			name: "parent defer, Cleanup only inside a subtest",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, _ := postgres.NewDB(cfg, tel)
	defer cleanup()
	t.Run("a", func(t *testing.T) {
		t.Cleanup(func() { check(db.Exec("x").Error) })
	})
}`,
			want: []string{"test-cleanup-pool-defer@004"},
		},
		{
			name: "defer of something that is not a pool closer",
			src: `package x_test
func TestX(t *testing.T) {
	_, telCleanup, _ := otel.New(a, b)
	defer telCleanup()
	t.Cleanup(func() { check(db.Exec("x").Error) })
}`,
		},
		{
			name: "closing a connection in Cleanup is not a write",
			src: `package x_test
func TestX(t *testing.T) {
	t.Cleanup(func() { _ = conn.Close(ctx) })
}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, st := scanSrc(t, tc.src)
			if st.Files != 1 || st.Funcs == 0 {
				t.Fatalf("scanned %+v: the fixture was not looked at", st)
			}
			if len(tc.want) > 0 && len(found) == 0 {
				t.Fatalf("negative fixture produced no hit at all (stats %+v): the scanner is matching nothing", st)
			}
			if got := strings.Join(hits(found), ","); got != strings.Join(tc.want, ",") {
				t.Fatalf("findings = %s, want %s\n%v", got, strings.Join(tc.want, ","), found)
			}
		})
	}
}

// goWorkModules returns the repo root (the directory holding go.work) and the
// use directories listed there, relative to it.
func goWorkModules(t *testing.T) (root string, mods []string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found above the test's directory")
		}
		dir = parent
	}
	b, err := os.ReadFile(filepath.Join(dir, "go.work"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\./\S+)\s*$`).FindAllStringSubmatch(string(b), -1) {
		mods = append(mods, strings.TrimPrefix(m[1], "./"))
	}
	if len(mods) == 0 {
		t.Fatal("go.work lists no module")
	}
	return dir, mods
}

// ungated are the modules that do not run the gate, and why. kernel is the
// zero-dependency core: a test importing platform/archtest would make it require
// platform, turning the dependency around. tools/stripvalidate is a standalone
// build tool with no database test.
var ungated = map[string]string{
	"pkgs/kernel":         "zero-dependency shared kernel; must not require platform",
	"tools/stripvalidate": "build tool, no database tests, no platform dependency",
}

const gateFile = "pgtest_cleanup_test.go"

// CI runs a module's tests only when the change reaches that module
// (scripts/affected.sh), so a gate that lives in one module is not run by a PR
// that only touches another one's tests. It therefore lives in every module with
// database tests. This pins that every go.work module either has the gate file
// or is on the explicit ungated list, so a new module cannot opt out by silence.
func TestTestCleanup_EveryModuleRunsTheGate(t *testing.T) {
	root, mods := goWorkModules(t)
	gated := 0
	for _, m := range mods {
		if why, skip := ungated[m]; skip {
			if why == "" {
				t.Errorf("%s: ungated without a reason", m)
			}
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, m, gateFile))
		if err != nil {
			t.Errorf("%s: no %s; add it (archtest.RunTestCleanup) or list the module as ungated with a reason: %v", m, gateFile, err)
			continue
		}
		if !strings.Contains(string(b), `archtest.RunTestCleanup(t, ".")`) {
			t.Errorf(`%s/%s does not call archtest.RunTestCleanup(t, ".")`, m, gateFile)
		}
		gated++
	}
	for m := range ungated {
		listed := false
		for _, g := range mods {
			listed = listed || g == m
		}
		if !listed {
			t.Errorf("ungated lists %s, which go.work does not use: stale entry", m)
		}
	}
	if gated == 0 {
		t.Fatal("no module runs the gate")
	}
}

// The gate scans real code, and a scan that matches nothing is green. Take every
// real file that binds a NewDB closer, break it the way the bug is written (a
// defer next to a t.Cleanup), and require the scanner to hit each one: the count
// of broken files must be above zero and every one of them must hit.
func TestTestCleanup_RealFilesHitWhenBroken(t *testing.T) {
	root, mods := goWorkModules(t)
	closerRE := regexp.MustCompile(`(?m)^\s*\w+,\s*(\w+),\s*(?:err|_)\s*:?=\s*\w*\.?NewDB\(`)
	var broken int
	var missed []string
	for _, m := range mods {
		err := filepath.WalkDir(filepath.Join(root, m), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(b)
			if filepath.Base(path) == "testcleanup_test.go" {
				return nil // its fixtures are source text, not code
			}
			loc := closerRE.FindStringSubmatchIndex(src)
			if loc == nil {
				return nil
			}
			name := src[loc[2]:loc[3]]
			after := loc[1] // the first release after the binding is the one that belongs to it
			// However the closer is released today, release it with a defer and
			// register a t.Cleanup next to it.
			mutated := ""
			for _, form := range []string{"defer " + name + "()", "t.Cleanup(" + name + ")"} {
				if i := strings.Index(src[after:], form); i >= 0 {
					i += after
					mutated = src[:i] + "defer " + name + "(); t.Cleanup(func() {})" + src[i+len(form):]
					break
				}
			}
			if mutated == "" {
				return nil // released some other way; nothing to break here
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, mutated, 0)
			if perr != nil {
				t.Fatalf("%s: mutated source does not parse: %v", path, perr)
			}
			broken++
			found, _ := archtest.ScanTestCleanupFiles(fset, []*ast.File{f})
			hit := false
			for _, x := range found {
				hit = hit || x.Rule == archtest.RulePoolDefer
			}
			if !hit {
				missed = append(missed, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if broken == 0 {
		t.Fatal("no real file binds a NewDB closer that could be broken: the check observed nothing")
	}
	if len(missed) > 0 {
		t.Fatalf("the gate missed a broken real file: %v", missed)
	}
}

// ── shape matrix ─────────────────────────────────────────────────────────────
//
// The rule is a SHAPE BAN, not a lifecycle proof: a test function (with every
// closure and t.Run body inside it) that defers the close of a NewDB pool and
// registers a t.Cleanup anywhere is reported, whichever t the Cleanup is on and
// whether it would run before or after the close. The gate does not try to decide
// that from the syntax (which t a closure was handed, an alias, which parameter a
// helper registers on, a Run method that is not testing's): the shapes are
// unbounded and two review rounds each found another. The fix is the same for all
// of them (pgtest.DB), so a child that happens to be safe is reported and migrated
// too. This deliberately tightens #371, which left safe subtest shapes alone.
//
// Every row below must hit. Rows are {mechanism} x {direct t.Cleanup, helper that
// registers t.Cleanup}; the helper is declared in a second file of the same
// package (cross-file discovery). The controls after it must not.
var shapeCells = []struct{ name, src string }{
	{"same_level_direct", `package probe_test

func TestProbe_same_level_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } })
}
`},
	{"same_level_helper", `package probe_test

func TestProbe_same_level_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	registerPurge(t, db)
}
`},
	{"t_run_child_direct", `package probe_test

func TestProbe_t_run_child_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("child", func(t *testing.T) { t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } }) })
}
`},
	{"t_run_child_helper", `package probe_test

func TestProbe_t_run_child_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("child", func(t *testing.T) { registerPurge(t, db) })
}
`},
	{"immediate_closure_parent_t_direct", `package probe_test

func TestProbe_immediate_closure_parent_t_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	func(t *testing.T) { t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } }) }(t)
}
`},
	{"immediate_closure_parent_t_helper", `package probe_test

func TestProbe_immediate_closure_parent_t_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	func(t *testing.T) { registerPurge(t, db) }(t)
}
`},
	{"immediate_closure_renamed_t_direct", `package probe_test

func TestProbe_immediate_closure_renamed_t_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	func(tt *testing.T) { tt.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { tt.Errorf("purge: %v", err) } }) }(t)
}
`},
	{"immediate_closure_renamed_t_helper", `package probe_test

func TestProbe_immediate_closure_renamed_t_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	func(tt *testing.T) { registerPurge(tt, db) }(t)
}
`},
	{"capturing_closure_direct", `package probe_test

func TestProbe_capturing_closure_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	func() { t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } }) }()
}
`},
	{"capturing_closure_helper", `package probe_test

func TestProbe_capturing_closure_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	func() { registerPurge(t, db) }()
}
`},
	{"forwarded_t_direct", `package probe_test

func TestProbe_forwarded_t_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	u := t
	func() { u.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { u.Errorf("purge: %v", err) } }) }()
}
`},
	{"forwarded_t_helper", `package probe_test

func TestProbe_forwarded_t_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	u := t
	registerPurge(u, db)
}
`},
	{"named_subtest_func_direct", `package probe_test

var probeDB_named_subtest_func_direct *gorm.DB

func probe_named_subtest_func_direct_child(t *testing.T) {
	db := probeDB_named_subtest_func_direct
	t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } })
}

func TestProbe_named_subtest_func_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	probeDB_named_subtest_func_direct = db
	t.Run("child", probe_named_subtest_func_direct_child)
}
`},
	{"named_subtest_func_helper", `package probe_test

var probeDB_named_subtest_func_helper *gorm.DB

func probe_named_subtest_func_helper_child(t *testing.T) { registerPurge(t, probeDB_named_subtest_func_helper) }

func TestProbe_named_subtest_func_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	probeDB_named_subtest_func_helper = db
	t.Run("child", probe_named_subtest_func_helper_child)
}
`},
	{"runner_run_is_not_a_subtest", `package probe_test

type probe_runner_run_is_not_a_subtest_runner struct{ t *testing.T }

// Run runs fn at once with the SAME t: not a subtest.
func (r probe_runner_run_is_not_a_subtest_runner) Run(name string, fn func(*testing.T)) { fn(r.t) }

func TestProbe_runner_run_is_not_a_subtest(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	r := probe_runner_run_is_not_a_subtest_runner{t}
	r.Run("x", func(t *testing.T) { t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } }) })
}
`},
	{"helper_registers_on_parent_param", `package probe_test

func TestProbe_helper_registers_on_parent_param(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("child", func(ct *testing.T) { registerOwner(t, ct, db) })
}
`},
	{"child_alias_direct", `package probe_test

func TestProbe_child_alias_direct(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("child", func(ct *testing.T) {
		u := ct
		u.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { u.Errorf("purge: %v", err) } })
	})
}
`},
	{"child_alias_helper", `package probe_test

func TestProbe_child_alias_helper(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("child", func(ct *testing.T) {
		u := ct
		registerPurge(u, db)
	})
}
`},
	{"grandchild", `package probe_test

func TestProbe_grandchild(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("a", func(a *testing.T) {
		a.Run("b", func(b *testing.T) { b.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { b.Errorf("purge: %v", err) } }) })
	})
}
`},
	{"child_captured_by_closure_in_child", `package probe_test

func TestProbe_child_captured_by_closure_in_child(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Run("c", func(ct *testing.T) {
		func() { ct.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { ct.Errorf("purge: %v", err) } }) }()
	})
}
`},
}

const shapeHelper = `package probe_test

func registerPurge(t *testing.T, db *gorm.DB) {
	t.Helper()
	t.Cleanup(func() {
		if err := db.Exec("SELECT 1").Error; err != nil {
			t.Errorf("purge: %v", err)
		}
	})
}

// registerOwner registers on its FIRST argument; the second is only passed along.
func registerOwner(owner, other *testing.T, db *gorm.DB) {
	owner.Helper()
	other.Helper()
	owner.Cleanup(func() {
		if err := db.Exec("SELECT 1").Error; err != nil {
			owner.Errorf("purge: %v", err)
		}
	})
}
`

// Controls that must NOT hit: the clean pgtest.DB use, a defer with no Cleanup,
// a Cleanup with no defer.
var shapeControlsClean = map[string]string{
	"clean_pgtest_db": `package probe_test

func TestProbe_clean(t *testing.T) {
	db := pgtest.DB(t, cfg, tel)
	t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } })
	registerPurge(t, db)
	t.Run("child", func(t *testing.T) { t.Cleanup(func() {}) })
}
`,
	"defer_only": `package probe_test

func TestProbe_deferOnly(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	if err := db.Exec("SELECT 1").Error; err != nil { t.Fatal(err) }
}
`,
	"cleanup_only": `package probe_test

func TestProbe_cleanupOnly(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	t.Cleanup(closeDB)
	t.Cleanup(func() { if err := db.Exec("SELECT 1").Error; err != nil { t.Errorf("purge: %v", err) } })
}
`,
}

// shapeControlHit is the plain shape the gate exists for; it must hit in the same run.
const shapeControlHit = `package probe_test

func TestProbe_control(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	t.Cleanup(func() { _ = db.Exec("SELECT 1").Error })
}
`

// The scanner is entered through its directory walk, as a module's gate is.
func TestTestCleanup_ShapeMatrix(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+"_test.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("helper", shapeHelper)
	write("control_hit", shapeControlHit)
	for _, c := range shapeCells {
		write(c.name, c.src)
	}
	for name, src := range shapeControlsClean {
		write(name, src)
	}

	found, st, err := archtest.ScanTestCleanupDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(shapeCells) + len(shapeControlsClean) + 2; st.Files != want {
		t.Fatalf("scanned %d files, want %d: the matrix was not looked at", st.Files, want)
	}
	hits := map[string]int{}
	for _, f := range found {
		if f.Rule == archtest.RulePoolDefer {
			hits[strings.TrimSuffix(filepath.Base(f.Pos.Filename), "_test.go")]++
		}
	}
	if hits["control_hit"] == 0 {
		t.Fatalf("the plain defer+Cleanup control did not hit: %v", found)
	}
	for _, c := range shapeCells {
		if hits[c.name] == 0 {
			t.Errorf("%s: not reported", c.name)
		}
	}
	for name := range shapeControlsClean {
		if hits[name] != 0 {
			t.Errorf("control %s: reported %d pool-defer hit(s), want none", name, hits[name])
		}
	}
	if len(shapeCells) < 14 {
		t.Fatalf("only %d matrix rows", len(shapeCells))
	}
}
