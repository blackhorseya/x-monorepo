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
		// ── direct-pool: a test may not open a pool itself; pgtest.DB does ──────────
		{
			name: "NewDB called",
			src: `package x_test
func TestX(t *testing.T) {
	db, cleanup, err := platformpg.NewDB(cfg, tel)
	defer cleanup()
}`,
			want: []string{"test-direct-pool@003"},
		},
		{
			name: "NewDB under another import name, and a dot import",
			src: `package x_test
func TestX(t *testing.T) {
	a, ca, _ := pg.NewDB(cfg, tel)
	b, cb, _ := NewDB(cfg, tel)
}`,
			want: []string{"test-direct-pool@003", "test-direct-pool@004"},
		},
		{
			name: "NewDB assigned to a variable and called through it",
			src: `package x_test
func TestX(t *testing.T) {
	open := postgres.NewDB
	db, closeDB, _ := open(cfg, tel)
}`,
			want: []string{"test-direct-pool@003"},
		},
		{
			name: "NewDB passed as an argument or kept in a struct field",
			src: `package x_test
func TestX(t *testing.T) {
	run(postgres.NewDB)
	_ = opener{open: postgres.NewDB}
}`,
			want: []string{"test-direct-pool@003", "test-direct-pool@004"},
		},
		{
			name: "other pool and connection openers",
			src: `package x_test
func TestX(t *testing.T) {
	a, _ := sql.Open("pgx", dsn)
	b := sql.OpenDB(connector)
	c, _ := pgxpool.New(ctx, dsn)
	d, _ := pgxpool.NewWithConfig(ctx, pc)
	e, _ := pgx.Connect(ctx, dsn)
	f, _ := pgx.ConnectConfig(ctx, cc)
	g, _ := gorm.Open(dialector, gc)
}`,
			want: []string{
				"test-direct-pool@003", "test-direct-pool@004", "test-direct-pool@005", "test-direct-pool@006",
				"test-direct-pool@007", "test-direct-pool@008", "test-direct-pool@009",
			},
		},
		{
			name: "openers under another import name",
			src: `package x_test
import (
	dbsql "database/sql"
	pool "github.com/jackc/pgx/v5/pgxpool"
	pg "github.com/jackc/pgx/v5"
	orm "gorm.io/gorm"
	std "github.com/jackc/pgx/v5/stdlib"
	pgc "github.com/jackc/pgx/v5/pgconn"
)
func TestX(t *testing.T) {
	_, _ = dbsql.Open("pgx", dsn)
	_, _ = pool.New(ctx, dsn)
	_, _ = pg.Connect(ctx, dsn)
	_, _ = orm.Open(dialector, gc)
	_ = std.OpenDB(cc)
	_, _ = pgc.Connect(ctx, dsn)
}`,
			want: []string{
				"test-direct-pool@012", "test-direct-pool@013", "test-direct-pool@014",
				"test-direct-pool@015", "test-direct-pool@016", "test-direct-pool@017",
			},
		},
		{
			// A dot import makes the opener a bare identifier the gate cannot tell from a local
			// function, so the import itself is the finding.
			name: "dot imports of opener packages",
			src: `package x_test
import (
	. "database/sql"
	. "github.com/jackc/pgx/v5/pgxpool"
)
func TestX(t *testing.T) {
	_, _ = Open("pgx", dsn)
	_, _ = New(ctx, dsn)
}`,
			want: []string{"test-direct-pool@003", "test-direct-pool@004"},
		},
		{
			name: "a package that is not an opener, whatever it is called",
			src: `package x_test
import sql "example.com/other/sql"
func TestX(t *testing.T) {
	_, _ = sql.Open("x")
}`,
		},
		{
			// The 5 indirect shapes review r2 found against the defer+Cleanup pairing. None needs
			// to be understood: each one mentions NewDB, and that is the finding.
			name: "indirect close and registration shapes",
			src: `package x_test
func TestA(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	register := t.Cleanup
	register(func() {})
}
func TestB(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	finish := func() { closeDB() }
	defer finish()
	t.Cleanup(purge)
}
func TestC(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	finish := closeDB
	defer finish()
}
func TestD(t *testing.T) {
	db, closeDB, _ := postgres.NewDB(cfg, tel)
	defer closeDB()
	wrapper(t)
}`,
			want: []string{"test-direct-pool@003", "test-direct-pool@009", "test-direct-pool@015", "test-direct-pool@020"},
		},
		{
			name: "opened inside a closure, a t.Run body and a goroutine",
			src: `package x_test
func TestX(t *testing.T) {
	t.Run("a", func(t *testing.T) { _, c, _ := postgres.NewDB(cfg, tel); c() })
	go func() { _, c, _ := postgres.NewDB(cfg, tel); c() }()
}`,
			want: []string{"test-direct-pool@003", "test-direct-pool@004"},
		},

		// ── pool-close: the one way pgtest.DB's pool can still be closed by the test ─
		{
			name: "Close on the sql.DB behind a pgtest.DB",
			src: `package x_test
func TestX(t *testing.T) {
	db := pgtest.DB(t, cfg, tel)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	_ = sqlDB.Stats()
}`,
			want: []string{"test-pool-close@005"},
		},
		{
			name: "Close called outside a defer too",
			src: `package x_test
func TestX(t *testing.T) {
	db := pgtest.DB(t, cfg, tel)
	pool, err := db.DB()
	if err := pool.Close(); err != nil { t.Fatal(err) }
}`,
			want: []string{"test-pool-close@005"},
		},

		// ── cleanup-swallow ──────────────────────────────────────────────────────
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

		// ── what must stay green ────────────────────────────────────────────────
		{
			name: "pgtest.DB with a checked purge",
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
			name: "pgtest.TryDB and pgtest.Conn are not openers the gate bans",
			src: `package x_test
func TestX(t *testing.T) {
	db, err := pgtest.TryDB(t, cfg, tel)
	conn := pgtest.Conn(t, dsn)
	_, _ = db, conn
}`,
		},
		{
			name: "using the sql.DB without closing it",
			src: `package x_test
func TestX(t *testing.T) {
	db := pgtest.DB(t, cfg, tel)
	sqlDB, _ := db.DB()
	_, _ = sqlDB.ExecContext(ctx, "x")
	_ = sqlDB.Stats()
}`,
		},
		{
			name: "closing a connection in Cleanup is not a write",
			src: `package x_test
func TestX(t *testing.T) {
	t.Cleanup(func() { _ = conn.Close(ctx) })
}`,
		},
		{
			name: "the word NewDB in a comment or a string is not a mention",
			src: `package x_test
// NewDB is what pgtest.DB calls.
func TestX(t *testing.T) {
	t.Log("NewDB")
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

// The gate scans real code, and a scan that matches nothing is green. In every
// gated module, take a real *_test.go, add a mention of NewDB to its first test
// function, and require the scanner to hit it: the number of broken files must
// equal the number of gated modules (non-zero), and each one must hit.
func TestTestCleanup_RealFilesHitWhenBroken(t *testing.T) {
	root, mods := goWorkModules(t)
	testFunc := regexp.MustCompile(`(?m)^func Test\w+\([a-z]+ \*testing\.T\) \{\n`)
	var broken, gated int
	var missed []string
	for _, m := range mods {
		if _, skip := ungated[m]; skip {
			continue
		}
		gated++
		done := false
		err := filepath.WalkDir(filepath.Join(root, m), func(path string, d fs.DirEntry, err error) error {
			if err != nil || done || d.IsDir() || !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "testcleanup_test.go" {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			loc := testFunc.FindIndex(b)
			if loc == nil {
				return nil
			}
			mutated := string(b[:loc[1]]) + "\t_ = NewDB\n" + string(b[loc[1]:])
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, mutated, 0)
			if perr != nil {
				t.Fatalf("%s: mutated source does not parse: %v", path, perr)
			}
			done = true
			broken++
			found, _ := archtest.ScanTestCleanupFiles(fset, []*ast.File{f})
			hit := false
			for _, x := range found {
				hit = hit || x.Rule == archtest.RuleDirectPool
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
	if broken == 0 || broken != gated {
		t.Fatalf("broke %d real files in %d gated modules: the check observed nothing, or skipped a module", broken, gated)
	}
	if len(missed) > 0 {
		t.Fatalf("the gate missed a broken real file: %v", missed)
	}
}

// ── shape matrix ─────────────────────────────────────────────────────────────
//
// The rule is "a test does not open a pool itself": any MENTION of an opener
// (NewDB, sql.Open, pgxpool.New..., pgx.Connect...) is a finding, matched on
// identifiers and selectors, with no data flow. Calling it, assigning it, passing
// it, keeping it in a struct, aliasing the import: all the same finding, so there
// is no closer to defer, alias or wrap, and the earlier rule that paired a deferred
// close with a t.Cleanup (and needed alias tracking to be sound) is gone. The fix
// is pgtest.DB / pgtest.TryDB / pgtest.Conn.
//
// Every row must hit. The rows are the shapes the earlier pairing rule was bypassed
// or fooled by. The controls after them must not.
var shapeCells = []struct{ name, src string }{
	{"defer_close", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tt.Cleanup(func() {})\n}\n"},
	{"method_value_register", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tregister := t.Cleanup\n\tregister(func() {})\n}\n"},
	{"registrar_alias", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tregister := registerPurge\n\tregister(t, db)\n}\n"},
	{"deferred_closure", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tfinish := func() { closeDB() }\n\tdefer finish()\n\tt.Cleanup(func() {})\n}\n"},
	{"closer_alias", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tfinish := closeDB\n\tdefer finish()\n\tt.Cleanup(func() {})\n}\n"},
	{"wrapper_passes_registrar", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\treviewWrapper(t)\n}\n"},
	{"t_run_child", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tt.Run(\"child\", func(t *testing.T) { t.Cleanup(func() {}) })\n}\n"},
	{"named_subtest_func", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tt.Run(\"child\", probeChild)\n}\nfunc probeChild(t *testing.T) { t.Cleanup(func() {}) }\n"},
	{"immediate_closure_parent_t", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tfunc(t *testing.T) { t.Cleanup(func() {}) }(t)\n}\n"},
	{"runner_run_is_not_a_subtest", "package probe_test\ntype runner struct{ t *testing.T }\nfunc (r runner) Run(name string, fn func(*testing.T)) { fn(r.t) }\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\trunner{t}.Run(\"x\", func(t *testing.T) { t.Cleanup(func() {}) })\n}\n"},
	{"helper_registers_on_parent_param", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tt.Run(\"child\", func(ct *testing.T) { registerOwner(t, ct, db) })\n}\n"},
	{"child_alias", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n\tt.Run(\"child\", func(ct *testing.T) { u := ct; u.Cleanup(func() {}) })\n}\n"},
	{"no_cleanup_at_all", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tdefer closeDB()\n}\n"},
	{"closer_registered_with_cleanup", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tt.Cleanup(closeDB)\n}\n"},
	{"method_value_opener", "package probe_test\nfunc TestProbe(t *testing.T) {\n\topen := postgres.NewDB\n\tdb, closeDB, _ := open(cfg, tel)\n\tt.Cleanup(closeDB)\n}\n"},
	{"import_alias", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := pg.NewDB(cfg, tel)\n\tt.Cleanup(closeDB)\n}\n"},
	{"sql_open", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, _ := sql.Open(\"pgx\", dsn)\n\tt.Cleanup(func() { _ = db.Close() })\n}\n"},
	{"pgxpool_new", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tp, _ := pgxpool.New(ctx, dsn)\n\tt.Cleanup(p.Close)\n}\n"},
	{"pgx_connect", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tc, _ := pgx.Connect(ctx, dsn)\n\tt.Cleanup(func() { _ = c.Close(ctx) })\n}\n"},
	{"gorm_open", "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, _ := gorm.Open(dialector, gc)\n\t_ = db\n}\n"},
}

// Controls that must NOT hit, and the one that must (same content, allowlisted and not).
var shapeControlsClean = map[string]string{
	"clean_pgtest_db":     "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb := pgtest.DB(t, cfg, tel)\n\tt.Cleanup(func() { if err := db.Exec(\"SELECT 1\").Error; err != nil { t.Errorf(\"purge: %v\", err) } })\n\tregisterPurge(t, db)\n\tt.Run(\"child\", func(t *testing.T) { t.Cleanup(func() {}) })\n}\n",
	"pgtest_try_and_conn": "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, err := pgtest.TryDB(t, cfg, tel)\n\tc := pgtest.Conn(t, dsn)\n\t_, _, _ = db, err, c\n}\n",
}

const opensAPool = "package probe_test\nfunc TestProbe(t *testing.T) {\n\tdb, closeDB, _ := postgres.NewDB(cfg, tel)\n\tt.Cleanup(closeDB)\n}\n"

// The scanner is entered through its directory walk, as a module's gate is.
func TestTestCleanup_ShapeMatrix(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("m/helper_test.go", "package probe_test\nfunc registerPurge(t *testing.T, db *gorm.DB) { t.Cleanup(func() {}) }\n")
	write("m/control_hit_test.go", opensAPool)
	for _, c := range shapeCells {
		write("m/"+c.name+"_test.go", c.src)
	}
	for name, src := range shapeControlsClean {
		write("m/"+name+"_test.go", src)
	}
	// Same content, two places: listed in the allowlist, and not.
	write("allowed/opens_test.go", opensAPool)
	write("notallowed/opens_test.go", opensAPool)
	// pgtest's own package (the canonical path only) is exempt by construction, not by the
	// allowlist; a directory that merely has the same name is not it.
	write("pkgs/platform/postgres/pgtest/own_test.go", opensAPool)
	write("nested/pgtest/imposter_test.go", opensAPool)
	write("other/pkgs/platform/postgres/pgtest/imposter_test.go", opensAPool)

	allow := []archtest.Allow{{Path: "allowed/", Reason: "fixture: stands in for a test of NewDB itself"}}
	found, st, err := archtest.ScanTestCleanupDirAllow(dir, allow)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(shapeCells) + len(shapeControlsClean) + 7; st.Files != want {
		t.Fatalf("scanned %d files, want %d: the matrix was not looked at", st.Files, want)
	}
	count := map[string]int{}
	for _, f := range found {
		if f.Rule == archtest.RuleDirectPool {
			rel, _ := filepath.Rel(dir, f.Pos.Filename)
			count[filepath.ToSlash(rel)]++
		}
	}
	if count["m/control_hit_test.go"] == 0 {
		t.Fatalf("the plain NewDB control did not hit: %v", found)
	}
	for _, c := range shapeCells {
		if count["m/"+c.name+"_test.go"] == 0 {
			t.Errorf("%s: not reported", c.name)
		}
	}
	for name := range shapeControlsClean {
		if n := count["m/"+name+"_test.go"]; n != 0 {
			t.Errorf("control %s: reported %d direct-pool hit(s), want none", name, n)
		}
	}
	if n := count["allowed/opens_test.go"]; n != 0 {
		t.Errorf("allowlisted path reported %d hit(s), want none", n)
	}
	if count["notallowed/opens_test.go"] == 0 {
		t.Errorf("the same content outside the allowlist did not hit")
	}
	if n := count["pkgs/platform/postgres/pgtest/own_test.go"]; n != 0 {
		t.Errorf("pgtest's own package reported %d hit(s), want none", n)
	}
	for _, imposter := range []string{"nested/pgtest/imposter_test.go", "other/pkgs/platform/postgres/pgtest/imposter_test.go"} {
		if count[imposter] == 0 {
			t.Errorf("%s: a directory named pgtest elsewhere got pgtest's exemption", imposter)
		}
	}
	if len(shapeCells) < 20 {
		t.Fatalf("only %d matrix rows", len(shapeCells))
	}
}

// The allowlist is part of the gate's contract: each line is one path and its
// reason, never a comment-based skip. This pins its shape so an entry cannot lose
// its reason.
func TestTestCleanup_AllowlistEntriesHaveReasons(t *testing.T) {
	for _, a := range archtest.DirectPoolAllowlist {
		if a.Path == "" || strings.TrimSpace(a.Reason) == "" {
			t.Errorf("allowlist entry %+v needs a path and a reason", a)
		}
		if strings.HasPrefix(a.Path, "/") || strings.Contains(a.Path, "..") {
			t.Errorf("allowlist path %q must be repo-relative", a.Path)
		}
	}
}
