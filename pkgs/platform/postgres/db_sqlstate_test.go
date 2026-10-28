package postgres_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// rejectingServer answers every connection's startup message with an
// ErrorResponse that has the given severity and SQLSTATE and server text
// carrying the secret marker, then hangs up. It is the "server" side of a
// login that fails, with fields a real Postgres would never send.
func rejectingServer(t *testing.T, severity, code string) (hostPort string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() { // fire-and-forget: ends when the listener closes in Cleanup
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = c.Close() }()
				var n [4]byte
				if _, err := io.ReadFull(c, n[:]); err != nil {
					return
				}
				size := binary.BigEndian.Uint32(n[:])
				if size < 4 || size > 65536 {
					return
				}
				if _, err := io.CopyN(io.Discard, c, int64(size-4)); err != nil {
					return
				}
				body := "S" + severity + "\x00C" + code + "\x00M" + secretMarker + " message\x00D" + secretMarker + " detail\x00\x00"
				head := make([]byte, 5)
				head[0] = 'E'
				binary.BigEndian.PutUint32(head[1:], uint32(len(body)+4))
				_, _ = c.Write(append(head, body...))
			}()
		}
	}()

	return l.Addr().String()
}

// What a server puts in the error's code and severity fields is not trusted
// either: only a real SQLSTATE (five characters of 0-9 and A-Z) and one of
// Postgres's own severities are carried. Anything else is dropped, and then the
// error is the category alone and errors.As finds no PgError.
func TestConnectionErrors_ServerFieldsAreValidated(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	m := postgres.Migration{Schema: "sqlstate", Files: fstest.MapFS{"0001_a.up.sql": {Data: []byte("SELECT 1;")}}}

	cases := []struct {
		name, severity, code string
		wantCode, wantSev    string // "" = not carried
	}{
		{name: "a real login failure", severity: "FATAL", code: "28P01", wantCode: "28P01", wantSev: "FATAL"},
		{name: "secret in the code field", severity: "FATAL", code: secretMarker},
		{name: "lowercase", severity: "FATAL", code: "28p01"},
		{name: "too short", severity: "FATAL", code: "2800"},
		{name: "too long", severity: "FATAL", code: "28P010"},
		{name: "non-ASCII", severity: "FATAL", code: "28Pé1"},
		{name: "secret in the severity field", severity: secretMarker, code: "28P01", wantCode: "28P01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := rejectingServer(t, tc.severity, tc.code)
			cfg := config.Postgres{URL: "postgres://synthetic_user:synthetic_password@" + addr + "/synthetic_db?sslmode=disable&connect_timeout=1"}
			for entry, run := range map[string]func() error{
				"NewDB": func() error {
					_, cleanup, err := postgres.NewDB(cfg, tel)
					if cleanup != nil {
						cleanup()
					}

					return err
				},
				"migrate Up": func() error { return postgres.Up(context.Background(), cfg, m) },
			} {
				err := run()
				if err == nil {
					t.Fatalf("%s = nil, want a rejected login", entry)
				}
				if strings.Contains(err.Error(), secretMarker) {
					t.Fatalf("%s: the error carries server text: %q", entry, err)
				}
				var pgErr *pgconn.PgError
				as := errors.As(err, &pgErr)
				if tc.wantCode == "" {
					if as || strings.Contains(err.Error(), "SQLSTATE") {
						t.Fatalf("%s: an invalid SQLSTATE %q was carried: %q (As=%v)", entry, tc.code, err, as)
					}

					continue
				}
				if !as || pgErr.Code != tc.wantCode || pgErr.Severity != tc.wantSev || pgErr.Message != "" || pgErr.Detail != "" {
					t.Fatalf("%s: errors.As PgError = %+v (%v), want code %q severity %q and no text", entry, pgErr, err, tc.wantCode, tc.wantSev)
				}
			}
		})
	}
}
