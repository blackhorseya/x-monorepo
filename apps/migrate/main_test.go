package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// loadFromEnv is config.Load as main uses it (APP_* from the test's Setenv).
func loadFromEnv() (*config.AppConfig, error) { return config.Load() }

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestRun_Usage(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("no subcommand should fail")
	}
	if err := run([]string{"sideways"}); err == nil {
		t.Fatal("unknown command should fail")
	}
	if err := run([]string{"up", "extra"}); err == nil {
		t.Fatal("extra args should fail")
	}
	t.Setenv("APP_POSTGRES_URL", "")
	if err := run([]string{"up"}); err == nil || !strings.Contains(err.Error(), "postgres.url") {
		t.Fatalf("missing postgres.url: %v", err)
	}
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("--help: %v", err)
	}
}

// Integration: a throwaway Postgres container, or TEST_DATABASE_URL (see pgtest).
func TestRun_UpVersionDown(t *testing.T) {
	url := pgtest.URL(t)
	t.Setenv("APP_POSTGRES_URL", url)

	for _, cmd := range []string{"up", "up", "down", "up"} { // second up: already current is fine
		if err := run([]string{cmd}); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}

	// version prints one line per BC, every schema current and clean (none at all while the template ships no BC).
	var out bytes.Buffer
	cmd := newRootCmd(loadFromEnv)
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	if text := strings.TrimSpace(out.String()); text != "" {
		lines = strings.Split(text, "\n")
	}
	if len(lines) != len(migrations) {
		t.Fatalf("version printed %d lines, want %d:\n%s", len(lines), len(migrations), out.String())
	}
	for i, m := range migrations {
		if !strings.HasPrefix(lines[i], m.Schema+"\t") || !strings.HasSuffix(lines[i], "dirty=false") || strings.Contains(lines[i], "\t0\t") {
			t.Fatalf("version line %q for %s: want current and clean", lines[i], m.Schema)
		}
	}
}

// A schema this binary does not know (the identity BC's, in a database the removed migration had been applied to) is
// not touched: up, version and down neither fail nor drop it nor change its version table.
func TestRun_LeavesAnUnknownSchemaAlone(t *testing.T) {
	url := pgtest.URL(t)
	t.Setenv("APP_POSTGRES_URL", url)
	c := context.Background()
	conn := pgtest.Conn(t, url)
	// The database is shared by the package's tests (and by -count=2): drop the schema before the connection closes.
	t.Cleanup(func() {
		if _, err := conn.Exec(c, "DROP SCHEMA IF EXISTS leftover CASCADE"); err != nil {
			t.Errorf("drop leftover: %v", err)
		}
	})
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS leftover CASCADE",
		"CREATE SCHEMA leftover",
		"CREATE TABLE leftover.schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)",
		"INSERT INTO leftover.schema_migrations VALUES (3, false)",
	} {
		if _, err := conn.Exec(c, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	for _, cmd := range []string{"up", "version", "down", "up"} {
		if err := run([]string{cmd}); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}

	var version int64
	var dirty bool
	if err := conn.QueryRow(c, "SELECT version, dirty FROM leftover.schema_migrations").Scan(&version, &dirty); err != nil || version != 3 || dirty {
		t.Fatalf("leftover.schema_migrations = %d dirty=%v, %v; want it untouched (3, false)", version, dirty, err)
	}
}
