package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/acceptance"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Databases of the acceptance harness. Every URL a child or the rpc connects with is checked twice:
// by form before anything is built from it (checkBaseURL), and by asking the server which database that
// exact URL reaches (checkIdentity). The second is the guarantee; the first makes the error early and clear.

// allowedQuery is every query key a database URL may carry. dbname, database, service, options and the like
// can change which database (or what session) a URL reaches, so they are not here; a key a real setup needs
// is added on purpose, not accepted by default.
var allowedQuery = []string{"sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout", "application_name"}

// Nothing here may put the database URL, or a piece of it, in an error or a log line: it carries the password, and test output
// ends up in terminals and CI logs. Errors name a fixed category (and, for a query key, the key), never the string, and
// never wrap an error that holds it (*url.Error, pgx's ParseConfigError). pgconn's connect error is measured, not assumed: it
// prints "user=… database=…" and the address, never the password (TestSecret_UnreachableDatabase).

// checkBaseURL parses raw (TEST_DATABASE_URL or pgtest's container URL) and refuses what the harness cannot
// rename safely: anything but a postgres:// or postgresql:// URL (a keyword/value DSN included), and any
// query key outside allowedQuery.
func checkBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// err is a *url.Error holding the whole raw URL: say only that it does not parse.
		return nil, errors.New("database url does not parse (a password with special characters must be percent-encoded; a keyword/value DSN is not accepted)")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, errors.New("database url scheme must be postgres:// or postgresql:// (a keyword/value DSN is not accepted)")
	}
	for key := range u.Query() {
		if !slices.Contains(allowedQuery, key) {
			return nil, fmt.Errorf("database url query key %q is not accepted: only %s are, because the others can change which database it reaches", key, strings.Join(allowedQuery, ", "))
		}
	}
	return u, nil
}

// checkIdentity connects with exactly dbURL (the string the rpc and the worker get, through pgx) and requires
// current_database() to be want. It must run on t's own goroutine (pgtest.Conn fails the test), and the
// connection is closed before it returns: nothing may stay connected to a template or to a database about to be dropped.
func checkIdentity(t testing.TB, dbURL, want string) {
	t.Helper()
	c := context.Background()
	conn := pgtest.Conn(t, dbURL)
	var got string
	if err := conn.QueryRow(c, "SELECT current_database()").Scan(&got); err != nil {
		t.Fatalf("acceptance: %v", acceptance.DBFailure(c, "current_database() on database "+want, err))
	}
	if err := conn.Close(c); err != nil {
		t.Fatalf("acceptance: %v", acceptance.DBFailure(c, "close the identity connection to "+want, err))
	}
	if got != want {
		t.Fatalf("acceptance: the url for database %s reaches database %q", want, got)
	}
	t.Logf("acceptance: current_database() = %s", got)
}

// template is the migrated database every scenario's own database is copied from. All names carry a
// random run id, so two runs on the same server (-count=2, another package, another agent) never meet,
// and only what this run created is dropped.
type template struct {
	t    *testing.T
	conn *pgx.Conn // maintenance connection of the server (the database pgtest.URL points at)
	base *url.URL
	name string
	run  string

	dropWait time.Duration // how long drop waits for the backends of a closed connection to exit

	mu   sync.Mutex
	free []string        // checked URLs of databases made and not handed out yet
	live map[string]bool // databases this run made and has not dropped
}

func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// newTemplate creates the template database, applies apps/migrate to it, and registers its removal.
func newTemplate(t *testing.T) *template {
	t.Helper()
	base, err := checkBaseURL(pgtest.URL(t))
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	tpl := &template{t: t, conn: pgtest.Conn(t, base.String()), base: base, run: randomID(t), live: map[string]bool{}, dropWait: dropWait}
	tpl.name = "acc_tpl_" + tpl.run
	c := context.Background()

	// Cleanups run last in, first out: what is registered after the connection runs while it is still open.
	if _, err := tpl.conn.Exec(c, "CREATE DATABASE "+quote(tpl.name)); err != nil {
		t.Fatalf("acceptance: create template database (the role needs CREATEDB when TEST_DATABASE_URL is used): %v", acceptance.DBFailure(c, "CREATE DATABASE", err))
	}
	t.Cleanup(func() {
		tpl.dropRemaining()
		if _, err := tpl.conn.Exec(c, "DROP DATABASE "+quote(tpl.name)+" WITH (FORCE)"); err != nil {
			t.Errorf("acceptance: drop template database %s: %v", tpl.name, acceptance.DBFailure(c, "DROP DATABASE", err))
		}
	})

	tplURL, err := tpl.urlFor(tpl.name)
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	// The migrate below connects with tplURL: that string must reach the template, or the migrations land elsewhere.
	checkIdentity(t, tplURL, tpl.name)
	// apps/migrate holds the list of every BC's migrations and does not export it, so build and run it
	// (apps/worker's TestRun_SIGTERM does the same).
	bin := buildApp(t, t.TempDir(), "migrate")
	runStep(t, "migrate up", budget(t, migrateLimit), "", childEnv("APP_POSTGRES_URL="+tplURL, "APP_LOG_LEVEL=error"), bin, "up")

	// CREATE DATABASE ... TEMPLATE fails while any session is connected to the template. Nothing connects to it
	// from here on, and the migrate process is gone; assert that instead of hoping.
	if _, err := tpl.conn.Exec(c, "ALTER DATABASE "+quote(tpl.name)+" WITH ALLOW_CONNECTIONS false"); err != nil {
		t.Fatalf("acceptance: lock template database: %v", acceptance.DBFailure(c, "ALTER DATABASE", err))
	}
	if who, err := tpl.sessions(c, tpl.name); err != nil || who != "" {
		t.Fatalf("acceptance: template database %s still has sessions after migrate up: %q (%v)", tpl.name, who, acceptance.DBFailure(c, "list sessions", err))
	}
	return tpl
}

func quote(ident string) string { return pgx.Identifier{ident}.Sanitize() }

// urlFor is the base URL pointing at database db, checked to reach it as far as pgx can tell offline.
// checkIdentity then asks the server.
func (x *template) urlFor(db string) (string, error) {
	u := *x.base
	u.Path = "/" + db
	u.RawPath = ""
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return "", fmt.Errorf("the database url for %s is not a connection string pgx accepts", db) // err echoes the url
	}
	if cfg.Database != db {
		return "", fmt.Errorf("database url for %s would connect to %q", db, cfg.Database)
	}
	return u.String(), nil
}

// sessions describes every connection to db other than this one ("" when there is none).
func (x *template) sessions(c context.Context, db string) (string, error) {
	var who string
	err := x.conn.QueryRow(c, `SELECT coalesce(string_agg(format('pid %s: user=%s application_name=%s state=%s client=%s', pid, usename, application_name, state, coalesce(client_addr::text, 'local')), '; '), '')
		FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, db).Scan(&who)
	return who, err
}

// dropWait is how long drop waits for the backends of closed connections to exit before it calls them leftovers.
const dropWait = 5 * time.Second

var dbSeq atomic.Int64

// make creates one database from the template and checks that its URL reaches it. It runs on t's goroutine.
func (x *template) make() string {
	x.t.Helper()
	name := fmt.Sprintf("acc_%s_%d", x.run, dbSeq.Add(1))
	dbURL, err := x.urlFor(name)
	if err != nil {
		x.t.Fatalf("acceptance: %v", err)
	}
	q := "CREATE DATABASE " + quote(name) + " TEMPLATE " + quote(x.name)
	if _, err := x.conn.Exec(context.Background(), q); err != nil {
		x.t.Fatalf("acceptance: create database %s: %v", name, acceptance.DBFailure(context.Background(), "CREATE DATABASE", err))
	}
	x.mu.Lock()
	x.live[name] = true
	x.mu.Unlock()
	checkIdentity(x.t, dbURL, name)
	return dbURL
}

// prepare makes n databases up front, on the test's own goroutine (a godog hook runs on another one,
// where the checks above could not fail the test).
func (x *template) prepare(n int) {
	x.t.Helper()
	for range n {
		x.free = append(x.free, x.make())
	}
}

// take hands out the next prepared database.
func (x *template) take() (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.free) == 0 {
		return "", errors.New("no prepared database left: more scenarios ran than the feature files hold")
	}
	dbURL := x.free[0]
	x.free = x.free[1:]
	return dbURL, nil
}

// drop removes the database behind dbURL. Nothing may be connected to it by then (the rpc and everything a step
// registered with Env.Defer is closed first); a leftover session fails the scenario and says who it is, and the
// database is then removed with FORCE at the end of the run.
func (x *template) drop(c context.Context, dbURL string) error {
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		return errors.New("drop scenario database: the database url is not a connection string pgx accepts") // err echoes the url
	}
	name := cfg.Database
	x.mu.Lock()
	owned := x.live[name]
	x.mu.Unlock()
	if !owned || name == x.name {
		return fmt.Errorf("drop scenario database: %q is not a database this run made", name)
	}
	// A closer's Close returns before the server's backend has exited, so a connection that was closed can still be listed for a
	// moment: wait, within dropWait, for none to be left. What stays past the bound is something that was not closed, and it
	// is named (pid, user, application_name, state; no query text). FORCE stays for dropRemaining, never for a scenario's drop.
	_, err = acceptance.Eventually(c, "the connections to "+name+" to be gone", x.dropWait, func(c context.Context) (string, bool, error) {
		who, err := x.sessions(c, name)
		if err != nil {
			return "", false, acceptance.DBFailure(c, "list the sessions of "+name, err)
		}
		return who, who == "", nil
	})
	if err != nil {
		return fmt.Errorf("drop scenario database %s (something was not closed first): %w", name, err)
	}
	if _, err := x.conn.Exec(c, "DROP DATABASE "+quote(name)); err != nil {
		return fmt.Errorf("drop scenario database %s: %w", name, acceptance.DBFailure(c, "DROP DATABASE", err))
	}
	x.mu.Lock()
	delete(x.live, name)
	x.mu.Unlock()
	return nil
}

// dropRemaining removes, with FORCE, what the run made and no scenario dropped (databases never handed out, or
// kept by a failed drop). The failure that left them has been reported already; this only keeps the server clean.
func (x *template) dropRemaining() {
	x.mu.Lock()
	names := make([]string, 0, len(x.live))
	for n := range x.live {
		names = append(names, n)
	}
	x.mu.Unlock()
	for _, n := range names {
		if _, err := x.conn.Exec(context.Background(), "DROP DATABASE "+quote(n)+" WITH (FORCE)"); err != nil {
			x.t.Errorf("acceptance: drop leftover database %s: %v", n, acceptance.DBFailure(context.Background(), "DROP DATABASE", err))
		}
	}
}
