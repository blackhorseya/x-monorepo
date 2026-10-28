package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func okMigration() postgres.Migration {
	return postgres.Migration{
		Schema: "conn_errors",
		Files:  fstest.MapFS{"0001_init.up.sql": {Data: []byte("SELECT 1;")}, "0001_init.down.sql": {Data: []byte("SELECT 1;")}},
	}
}

// migrate's connection failures read like NewDB's: a category, and the pinned
// host:port when expect_target is set, nothing else from the URL. Both ways of
// opening it.
func TestMigrate_ConnectionErrorsCarryNoURLText(t *testing.T) {
	for _, expectTarget := range []string{"", "127.0.0.1:1"} {
		t.Run("expect_target="+expectTarget, func(t *testing.T) {
			err := postgres.Up(context.Background(), config.Postgres{
				URL:          "postgres://leakyuser:" + secretMarker + "@127.0.0.1:1/leakydb?sslmode=disable",
				ExpectTarget: expectTarget,
			}, okMigration())
			if err == nil {
				t.Fatal("Up = nil, want an error (nothing listens on port 1)")
			}
			assertNoURLText(t, err, "leakyuser", secretMarker, "leakydb")
			if got := strings.Contains(err.Error(), "127.0.0.1:1"); got != (expectTarget != "") {
				t.Fatalf("host:port present = %v with expect_target=%q: %q", got, expectTarget, err)
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("errors.Is(err, ECONNREFUSED) = false for %q", err)
			}
		})
	}
}

// Integration (pgtest): a rejected login keeps its SQLSTATE and loses the
// server's text, which names the role; once connected, a migration's own SQL
// error is left whole, because that is what the operator has to fix.
func TestMigrate_ServerErrors(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	base := pgtest.URL(t)
	admin, adminCleanup, err := postgres.NewDB(config.Postgres{URL: base}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer adminCleanup()
	const role = "leaky_role_migrate_errors"
	if err := admin.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'right'", role)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP ROLE IF EXISTS " + role) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	hostPort := net.JoinHostPort(u.Hostname(), u.Port())

	for _, expectTarget := range []string{"", hostPort} {
		t.Run("expect_target="+expectTarget, func(t *testing.T) {
			wrong := fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=wrong sslmode=disable",
				u.Hostname(), u.Port(), strings.TrimPrefix(u.Path, "/"), role)
			err := postgres.Up(context.Background(), config.Postgres{URL: wrong, ExpectTarget: expectTarget}, okMigration())
			if err == nil {
				t.Fatal("Up = nil, want a rejected login")
			}
			assertNoURLText(t, err, role, "wrong")
			if got := strings.Contains(err.Error(), hostPort); got != (expectTarget != "") {
				t.Fatalf("host:port present = %v with expect_target=%q: %q", got, expectTarget, err)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "28P01" || pgErr.Message != "" {
				t.Fatalf("errors.As PgError = %+v (%v), want SQLSTATE 28P01 and no message", pgErr, err)
			}

			bad := postgres.Migration{
				Schema: "conn_errors_bad",
				Files:  fstest.MapFS{"0001_bad.up.sql": {Data: []byte("SELECT * FROM table_that_does_not_exist_xyz;")}},
			}
			err = postgres.Up(context.Background(), config.Postgres{URL: base, ExpectTarget: expectTarget}, bad)
			t.Cleanup(func() { admin.Exec("DROP SCHEMA IF EXISTS conn_errors_bad CASCADE") })
			if err == nil || !strings.Contains(err.Error(), "table_that_does_not_exist_xyz") {
				t.Fatalf("a migration's own SQL error = %v, want its text kept", err)
			}
		})
	}
}

// Without expect_target no host from the URL is ever printed (see NewDB's
// TestNewDB_UnpinnedHostIsNeverPrinted), for up, down and version alike; and a
// URL that does not parse is reported in fixed words, not as pgx quotes it.
func TestMigrate_UnpinnedHostAndParseErrorsAreNotQuoted(t *testing.T) {
	m := okMigration()
	ops := map[string]func(cfg config.Postgres) error{
		"up":   func(cfg config.Postgres) error { return postgres.Up(context.Background(), cfg, m) },
		"down": func(cfg config.Postgres) error { return postgres.Down(context.Background(), cfg, m) },
		"version": func(cfg config.Postgres) error {
			_, _, err := postgres.Version(context.Background(), cfg, m)

			return err
		},
	}
	for name, op := range ops {
		t.Run(name+"/valid URL, unpinned host", func(t *testing.T) {
			err := op(config.Postgres{URL: "postgres://u:prefix@SYNTHETICSECRET.invalid:1/app?sslmode=disable"})
			if err == nil {
				t.Fatal("want an error")
			}
			assertNoURLText(t, err, "SYNTHETICSECRET", ".invalid")
		})
		for _, expectTarget := range []string{"", "127.0.0.1:1"} {
			t.Run(name+"/unparsable/expect_target="+expectTarget, func(t *testing.T) {
				err := op(config.Postgres{
					URL:          "postgres://leakyuser:" + secretMarker + "@h:notaport/leakydb",
					ExpectTarget: expectTarget,
				})
				if err == nil {
					t.Fatal("want an error")
				}
				assertNoURLText(t, err, "leakyuser", secretMarker, "leakydb", "notaport", "xxxxx")
				if !strings.Contains(err.Error(), "not a valid connection string") {
					t.Fatalf("error = %q, want the fixed parse message", err)
				}
			})
		}
	}
}
