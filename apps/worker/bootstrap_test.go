package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	identitypg "github.com/blackhorseya/x-monorepo/pkgs/identity/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): `worker bootstrap` on the real tables — the first run
// writes one account and one key and prints the secret to stdout only; the
// second run is a no-op. The log (process stdout / stderr, not the command's
// writer) never carries the secret.
func TestBootstrapCommand(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.AppConfig{Postgres: config.Postgres{URL: pgtest.URL(t)}, Log: config.Log{Level: "debug", Format: "text"}, Auth: config.Auth{AdminRole: "admin"}}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, dbCleanup, err := postgres.NewDB(cfg.Postgres, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer dbCleanup()
	if err := postgres.Up(c, cfg.Postgres, identitypg.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("TRUNCATE identity.api_keys, identity.accounts CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	count := func(tbl, where string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl + " WHERE " + where).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	// exec runs the command with the log captured too (a CLI use case logs
	// through slog.Default: nothing puts a logger in its context), so the test
	// can tell what went to the operator (out) from what went to the log.
	pc, err := pgx.ParseConfig(cfg.Postgres.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := net.JoinHostPort(pc.Host, strconv.Itoa(int(pc.Port)))
	exec := func(cfg *config.AppConfig, args ...string) (out, logged string, err error) {
		var buf, logBuf bytes.Buffer
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&buf)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"bootstrap", "--insecure-allow-plaintext", "--expect-target", target}, args...)) // the test database is plaintext, and the target is required
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		err = cmd.ExecuteContext(c)
		return buf.String(), logBuf.String(), err
	}

	// Argument errors never open the database.
	noDB := &config.AppConfig{Log: cfg.Log, Auth: cfg.Auth}
	for _, args := range [][]string{{"--name", " "}, {"--key-name", ""}, {"--expires-in", "-1h"}, {"extra"}} {
		if _, _, err := exec(noDB, args...); err == nil || strings.Contains(err.Error(), "postgres.url") {
			t.Fatalf("bootstrap %v without a database = %v, want a usage error", args, err)
		}
	}
	if _, _, err := exec(noDB); err == nil || !strings.Contains(err.Error(), "postgres.url") {
		t.Fatalf("bootstrap without a database = %v", err)
	}

	out, logged, err := exec(cfg, "--name", "ops", "--role", "admin", "--role", "ops", "--key-name", "first", "--expires-in", "720h")
	if err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}
	secret := ""
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "X-API-Key: "); ok {
			secret = rest
		}
	}
	if !strings.HasPrefix(secret, "xk_") || len(secret) != 3+43 || !strings.Contains(out, "account ") || !strings.Contains(out, "(ops) roles=[admin ops]") || !strings.Contains(out, "prefix="+secret[:8]) || !strings.Contains(out, "shown once") {
		t.Fatalf("bootstrap output:\n%s", out)
	}
	if strings.Contains(logged, "xk_") || strings.Contains(logged, secret[3:]) {
		t.Fatalf("the secret reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "identity.bootstrap") {
		t.Fatalf("the use case did not log (is the log captured?):\n%s", logged)
	}
	sum := sha256.Sum256([]byte(secret))
	if count("identity.accounts", "name = 'ops' AND status = 'ACTIVE' AND roles = '[\"admin\",\"ops\"]'::jsonb") != 1 ||
		count("identity.api_keys", "name = 'first' AND status = 'ACTIVE' AND expires_at IS NOT NULL AND hash = decode('"+hex(sum[:])+"', 'hex')") != 1 {
		t.Fatalf("rows after bootstrap: accounts=%d keys=%d", count("identity.accounts", "true"), count("identity.api_keys", "true"))
	}

	// Second run: nothing written, exit 0, no secret.
	out, _, err = exec(cfg, "--name", "other")
	if err != nil || !strings.Contains(out, "already bootstrapped") || strings.Contains(out, "xk_") {
		t.Fatalf("second bootstrap = %v\n%s", err, out)
	}
	if count("identity.accounts", "true") != 1 || count("identity.api_keys", "true") != 1 {
		t.Fatal("second bootstrap wrote rows")
	}

	// Defaults: name root, key bootstrap, role = auth.admin_role, never expires.
	if err := db.WithContext(c).Exec("TRUNCATE identity.api_keys, identity.accounts CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	if out, _, err = exec(cfg); err != nil || !strings.Contains(out, "(root) roles=[admin]") || !strings.Contains(out, "(bootstrap) prefix=") || !strings.Contains(out, "expires=never") {
		t.Fatalf("default bootstrap = %v\n%s", err, out)
	}
	if count("identity.api_keys", "expires_at IS NULL") != 1 {
		t.Fatal("default key should never expire")
	}
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&0x0f])
	}
	return string(out)
}

// Integration (pgtest): `worker bootstrap --admin-email` on the real tables —
// an admin account and an invite for that email, no API key, no secret in the
// output or the log; the second run is a no-op.
func TestBootstrapCommand_AdminEmail(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.AppConfig{Postgres: config.Postgres{URL: pgtest.URL(t)}, Log: config.Log{Level: "debug", Format: "text"}, Auth: config.Auth{AdminRole: "admin"}}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, dbCleanup, err := postgres.NewDB(cfg.Postgres, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer dbCleanup()
	if err := postgres.Up(c, cfg.Postgres, identitypg.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec("TRUNCATE identity.api_keys, identity.accounts CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	count := func(tbl, where string) int64 {
		var n int64
		if err := db.WithContext(c).Raw("SELECT count(*) FROM " + tbl + " WHERE " + where).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	pc, err := pgx.ParseConfig(cfg.Postgres.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := net.JoinHostPort(pc.Host, strconv.Itoa(int(pc.Port)))
	exec := func(args ...string) (out, logged string, err error) {
		var buf, logBuf bytes.Buffer
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&buf)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"bootstrap", "--insecure-allow-plaintext", "--expect-target", target}, args...))
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		err = cmd.ExecuteContext(c)
		return buf.String(), logBuf.String(), err
	}

	// The key flags belong to the API-key path; mixing them in is a usage error
	// that never opens the database.
	for _, args := range [][]string{{"--admin-email", "a@b.co", "--key-name", "k"}, {"--admin-email", "a@b.co", "--expires-in", "1h"}} {
		if _, _, err := exec(args...); err == nil || !strings.Contains(err.Error(), "--admin-email") {
			t.Fatalf("bootstrap %v = %v, want a usage error naming --admin-email", args, err)
		}
	}
	if count("identity.accounts", "true") != 0 {
		t.Fatal("a usage error wrote rows")
	}
	// A malformed email writes nothing.
	if _, _, err := exec("--admin-email", "not-an-email"); err == nil || count("identity.accounts", "true") != 0 {
		t.Fatalf("bad email = %v, accounts %d", err, count("identity.accounts", "true"))
	}

	out, logged, err := exec("--admin-email", "Admin@Example.com", "--name", "root")
	if err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(root) roles=[admin]") || !strings.Contains(out, "for admin@example.com") || strings.Contains(out, "X-API-Key") || strings.Contains(out, "xk_") {
		t.Fatalf("bootstrap output:\n%s", out)
	}
	if strings.Contains(logged, "xk_") || strings.Contains(logged, "X-API-Key") {
		t.Fatalf("a secret reached the log:\n%s", logged)
	}
	if count("identity.accounts", "name = 'root' AND status = 'ACTIVE' AND roles = '[\"admin\"]'::jsonb") != 1 ||
		count("identity.api_keys", "true") != 0 ||
		count("identity.invites", "email = 'admin@example.com' AND claimed_at IS NULL AND expires_at = created_at + interval '7 days'") != 1 {
		t.Fatalf("rows: accounts=%d keys=%d invites=%d", count("identity.accounts", "true"), count("identity.api_keys", "true"), count("identity.invites", "true"))
	}

	// Second run: nothing written, exit 0.
	out, _, err = exec("--admin-email", "admin@example.com")
	if err != nil || !strings.Contains(out, "already bootstrapped") {
		t.Fatalf("second bootstrap = %v\n%s", err, out)
	}
	if count("identity.accounts", "true") != 1 || count("identity.invites", "true") != 1 {
		t.Fatal("second bootstrap wrote rows")
	}
}
