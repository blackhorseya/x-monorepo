package postgres_test

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

const secretMarker = "SYNTHETIC_SECRET"

func assertNoURLText(t *testing.T, err error, frags ...string) {
	t.Helper()
	for _, f := range append(frags, "postgres://", "password=", "user=", "database=") {
		if strings.Contains(err.Error(), f) {
			t.Fatalf("the error carries %q from the URL or the server: %q", f, err)
		}
	}
}

// A failed connection tells the operator what kind of failure it was and
// nothing parsed from the URL: not the user, database or password, and not the
// text pgx or the server add around them.
func TestNewDB_ConnectionRefusedCarriesNoURLText(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	for _, expectTarget := range []string{"", "127.0.0.1:1"} {
		t.Run("expect_target="+expectTarget, func(t *testing.T) {
			_, cleanup, err := postgres.NewDB(config.Postgres{
				URL:          "postgres://leakyuser:" + secretMarker + "@127.0.0.1:1/leakydb?sslmode=disable",
				ExpectTarget: expectTarget,
			}, tel)
			if err == nil {
				cleanup()
				t.Fatal("NewDB = nil, want an error (nothing listens on port 1)")
			}
			assertNoURLText(t, err, "leakyuser", secretMarker, "leakydb")
			if !strings.Contains(err.Error(), "127.0.0.1:1") {
				t.Fatalf("the error lacks the host:port that was dialed: %q", err)
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("errors.Is(err, ECONNREFUSED) = false for %q: the category must stay checkable", err)
			}
		})
	}
}

// Integration (pgtest): a rejected login keeps its SQLSTATE for errors.As but
// not the server's message, which names the role; and a password that contains
// "TimeZone=<x>" is a password, not a session timezone.
func TestNewDB_PasswordIsNotATimeZoneAndServerTextIsDropped(t *testing.T) {
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
	const role = "leaky_role_conn_errors"
	password := "pTimeZone=" + secretMarker
	if err := admin.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, password)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP ROLE IF EXISTS " + role) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	dsn := func(pw string) string {
		return fmt.Sprintf("host=%s port=%s dbname=%s user=%s password=%s sslmode=disable",
			u.Hostname(), u.Port(), strings.TrimPrefix(u.Path, "/"), role, pw)
	}

	t.Run("password that looks like a TimeZone setting", func(t *testing.T) {
		db, cleanup, err := postgres.NewDB(config.Postgres{URL: dsn(password)}, tel)
		if err != nil {
			t.Fatalf("NewDB: %v (the password fragment was sent as the session timezone)", err)
		}
		defer cleanup()
		var tz string
		if err := db.Raw("SHOW timezone").Scan(&tz).Error; err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tz, secretMarker) {
			t.Fatalf("session timezone = %q", tz)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		_, cleanup, err := postgres.NewDB(config.Postgres{URL: dsn("wrong")}, tel)
		if err == nil {
			cleanup()
			t.Fatal("NewDB = nil, want a rejected login")
		}
		assertNoURLText(t, err, role, "wrong")
		if want := net.JoinHostPort(u.Hostname(), u.Port()); !strings.Contains(err.Error(), want) {
			t.Fatalf("the error lacks the host:port that was dialed (%s): %q", want, err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
			t.Fatalf("errors.As PgError = %v (%v), want SQLSTATE 28P01", pgErr, err)
		}
		if pgErr.Message != "" || pgErr.Detail != "" {
			t.Fatalf("the PgError still carries the server's text: %+v", pgErr)
		}
	})
}

// Several hosts: every target that was tried is named, none of the rest of the
// URL. A host that is not shaped like one (a password with an unencoded @ makes
// pgx cut part of it out as the host) is never printed, and then the error is
// the category alone.
func TestNewDB_ConnectionErrorNamesEveryTargetTried(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	_, cleanup, err := postgres.NewDB(config.Postgres{
		URL: "postgres://leakyuser:" + secretMarker + "@127.0.0.1:1,127.0.0.1:2/leakydb?sslmode=disable",
	}, tel)
	if err == nil {
		cleanup()
		t.Fatal("NewDB = nil, want an error")
	}
	assertNoURLText(t, err, "leakyuser", secretMarker, "leakydb")
	for _, want := range []string{"127.0.0.1:1", "127.0.0.1:2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error lacks %s: %q", want, err)
		}
	}

	_, cleanup, err = postgres.NewDB(config.Postgres{
		URL: "postgres://u:@" + secretMarker + "@127.0.0.1:1/app?sslmode=disable",
	}, tel)
	if err == nil {
		cleanup()
		t.Fatal("NewDB = nil, want an error")
	}
	assertNoURLText(t, err, secretMarker)
}
