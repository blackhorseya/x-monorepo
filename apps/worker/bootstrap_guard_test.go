package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

const guardPassword = "s3cretPW"

// writeTestCA writes a throwaway self-signed CA and returns its path, escaped
// for use as a sslrootcert query value.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "bootstrap-guard-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	return url.QueryEscape(path)
}

// The check runs on pgx's own parse of the URL — the parse gorm's postgres
// driver does for the real connection — so what it approves is what pgx will
// do, not what a shell or a regexp thinks the URL says. Every rejection names
// a reason and at most a host:port; the URL and the password never appear.
func TestCheckBootstrapConn(t *testing.T) {
	ca := writeTestCA(t)
	const base = "postgres://u:" + guardPassword + "@127.0.0.1:15432/app"
	forward := connPolicy{expectTarget: "127.0.0.1:15432"}

	cases := []struct {
		name     string
		url      string
		policy   connPolicy
		password string   // what must not leak; defaults to guardPassword
		leaks    []string // more fragments of the URL or environment the error must not carry
		env      map[string]string
		noTarget bool // run without --expect-target (the loop otherwise pins base's host:port)
		reject   bool
		contains string // part of the expected error
	}{
		// What passes: the certificate chain is verified.
		{name: "verify-ca with sslrootcert", url: base + "?sslmode=verify-ca&sslrootcert=" + ca},
		{name: "verify-full", url: base + "?sslmode=verify-full"},
		// pgx treats require + sslrootcert as verify-ca (configTLS), so the chain is verified.
		{name: "require with sslrootcert is verify-ca in pgx", url: base + "?sslmode=require&sslrootcert=" + ca},
		{name: "verify-ca to the expected target", url: base + "?sslmode=verify-ca&sslrootcert=" + ca, policy: forward},
		{name: "several hosts, every one the target", url: "postgres://u:" + guardPassword + "@127.0.0.1:15432,127.0.0.1:15432/app?sslmode=verify-full"},
		{name: "several hosts, they differ", url: "postgres://u:" + guardPassword + "@127.0.0.1:15432,b.example:5432/app?sslmode=verify-full", reject: true, contains: "does not match --expect-target 127.0.0.1:15432", leaks: []string{"b.example"}},

		// No TLS, or TLS that does not verify the server.
		{name: "disable", url: base + "?sslmode=disable", reject: true, contains: "TLS is disabled"},
		{name: "allow falls back to plaintext", url: base + "?sslmode=allow", reject: true},
		{name: "prefer falls back to plaintext", url: base + "?sslmode=prefer", reject: true},
		{name: "require without sslrootcert encrypts but does not verify", url: base + "?sslmode=require", reject: true, contains: "does not verify"},

		// Bypasses of the earlier shell guards.
		{name: "spaced key: pgx trims it, last wins", url: base + "?sslmode=verify-ca&sslmode =disable&sslrootcert=" + ca, reject: true, contains: "TLS is disabled"},
		{name: "leading-space key", url: base + "?sslmode=verify-ca& sslmode=disable&sslrootcert=" + ca, reject: true, contains: "TLS is disabled"},
		{name: "duplicate key, last wins", url: base + "?sslmode=verify-full&sslmode=disable", reject: true, contains: "TLS is disabled"},
		{name: "percent-encoded key", url: base + "?sslmode=verify-full&ssl%6Dode=disable", reject: true},
		{
			name: "sslmode text inside the password", url: "postgres://u:p_sslmode=verify-ca@127.0.0.1:15432/app?sslmode=disable",
			password: "p_sslmode=verify-ca", reject: true, contains: "TLS is disabled",
		},
		{
			name: "host override in the query", url: base + "?sslmode=verify-ca&sslrootcert=" + ca + "&host=evil.example",
			policy: forward, reject: true, contains: "does not match --expect-target 127.0.0.1:15432", leaks: []string{"evil.example"},
		},

		// The target.
		{name: "authority host is not the expected target", url: "postgres://u:" + guardPassword + "@db.internal:5432/app?sslmode=verify-ca&sslrootcert=" + ca, policy: forward, reject: true, contains: "connection target does not match --expect-target 127.0.0.1:15432", leaks: []string{"db.internal"}},
		{name: "one of several hosts is not the target", url: "postgres://u:" + guardPassword + "@127.0.0.1:15432,evil.example:5432/app?sslmode=verify-full", policy: forward, reject: true, contains: "does not match --expect-target", leaks: []string{"evil.example"}},
		{name: "the target's port must match too", url: "postgres://u:" + guardPassword + "@127.0.0.1:5432/app?sslmode=verify-full", policy: forward, reject: true, contains: "does not match --expect-target"},

		// The development escape hatch skips TLS only.
		{name: "plaintext allowed", url: base + "?sslmode=disable", policy: connPolicy{allowPlaintext: true}},
		{name: "plaintext allowed to the expected target", url: base + "?sslmode=disable", policy: connPolicy{allowPlaintext: true, expectTarget: "127.0.0.1:15432"}},
		{name: "plaintext allowed, wrong target", url: "postgres://u:" + guardPassword + "@db.internal:5432/app?sslmode=disable", policy: connPolicy{allowPlaintext: true, expectTarget: "127.0.0.1:15432"}, reject: true, contains: "does not match --expect-target 127.0.0.1:15432", leaks: []string{"db.internal"}},

		// A password with an unencoded @: pgx takes the text after the first @ as the
		// host, so the "host" is part of the password. Neither goes into a message.
		{
			name: "unencoded @ in the password, wrong target", url: "postgres://u:@SYNTHETIC_SECRET@127.0.0.1:15432/app?sslmode=verify-full",
			policy: forward, password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host",
		},
		{
			name: "unencoded @ in the password, plaintext", url: "postgres://u:pa@SYNTHETIC_SECRET@127.0.0.1:15432/app?sslmode=disable",
			policy: connPolicy{allowPlaintext: true}, password: "SYNTHETIC_SECRET", leaks: []string{"pa@"}, reject: true, contains: "invalid host",
		},

		// The host can come from the environment, not the URL.
		{
			name: "PGHOST decides the host", url: "postgres:///app?sslmode=verify-full", env: map[string]string{"PGHOST": "pghost.example"},
			policy: forward, reject: true, contains: "does not match --expect-target", leaks: []string{"pghost.example"},
		},
		{
			name: "PGHOST with a TLS that does not verify", url: "postgres:///app?sslmode=require", env: map[string]string{"PGHOST": "pghost.example"},
			policy: connPolicy{expectTarget: "pghost.example:5432"}, reject: true, contains: "does not verify", leaks: []string{"pghost.example"},
		},
		{
			name: "PGHOST that is not a host name", url: "postgres:///app?sslmode=verify-full", env: map[string]string{"PGHOST": "SYNTHETIC_PGHOST"},
			reject: true, contains: "invalid host", leaks: []string{"SYNTHETIC_PGHOST"},
		},

		// The host must be an IP, a DNS name or a unix socket directory, whether or
		// not --expect-target is set: a host pgx cut out of the password otherwise
		// reaches the resolver, whose error quotes it into the log.
		{
			name: "unencoded @ in the password, no --expect-target", url: "postgres://u:@SYNTHETIC_SECRET@127.0.0.1:15432/app?sslmode=verify-full",
			noTarget: true, password: "SYNTHETIC_SECRET", reject: true, contains: "--expect-target is required",
		},
		{name: "host with a slash", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET/x", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "host with a space", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET%20x", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "host with a percent", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET%2541", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "host with an encoded @", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET%40127.0.0.1", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "empty label", url: base + "?sslmode=verify-full&host=a..example", reject: true, contains: "invalid host"},
		{name: "label starts with a hyphen", url: base + "?sslmode=verify-full&host=-a.example", reject: true, contains: "invalid host"},
		{name: "label over 63 chars", url: base + "?sslmode=verify-full&host=" + strings.Repeat("a", 64) + ".example", reject: true, contains: "invalid host"},
		{
			name: "a bad host in a fallback", url: "postgres://u:" + guardPassword + "@a.example:5432,SYNTHETIC_SECRET@b:5432/app?sslmode=verify-full",
			password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host",
		},
		{name: "IPv6 host", url: "postgres://u:" + guardPassword + "@[::1]:5432/app?sslmode=verify-full", policy: connPolicy{expectTarget: "[::1]:5432"}},
		{name: "DNS name with hyphens and digits", url: "postgres://u:" + guardPassword + "@db-1.eu-west.example.com:5432/app?sslmode=verify-full", policy: connPolicy{expectTarget: "db-1.eu-west.example.com:5432"}},
		{name: "unix socket directory", url: base + "?host=/var/run/postgresql", policy: connPolicy{allowPlaintext: true, expectTarget: "/var/run/postgresql:15432"}},
		// A fully qualified name ends in a root dot; only that one trailing dot is allowed.
		{name: "localhost with a root dot", url: "postgres://u:" + guardPassword + "@localhost.:5432/app?sslmode=verify-full", policy: connPolicy{expectTarget: "localhost.:5432"}},
		{name: "FQDN with a root dot", url: "postgres://u:" + guardPassword + "@db.example.com.:5432/app?sslmode=verify-full", policy: connPolicy{expectTarget: "db.example.com.:5432"}},
		{name: "empty label in the middle", url: base + "?sslmode=verify-full&host=a..b", reject: true, contains: "invalid host"},
		{name: "a lone root dot", url: base + "?sslmode=verify-full&host=.", reject: true, contains: "invalid host"},
		{name: "two root dots", url: base + "?sslmode=verify-full&host=..", reject: true, contains: "invalid host"},
		{name: "two trailing dots", url: base + "?sslmode=verify-full&host=db.example.com..", reject: true, contains: "invalid host"},
		{name: "socket path with a space", url: base + "?host=/var/run/SYNTHETIC_SECRET%20x", policy: connPolicy{allowPlaintext: true}, password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},

		// --expect-target is required: whatever pgx parses as the host may be a piece
		// of a malformed password (a valid-looking label too), so only the target the
		// operator typed can be trusted.
		{name: "no --expect-target", url: base + "?sslmode=verify-full", noTarget: true, reject: true, contains: "--expect-target is required"},
		{name: "no --expect-target, plaintext allowed", url: base + "?sslmode=disable", policy: connPolicy{allowPlaintext: true}, noTarget: true, reject: true, contains: "--expect-target is required"},
		{name: "no --expect-target, unparsable URL", url: "postgres://u:" + guardPassword + "@h:notaport/app", noTarget: true, reject: true, contains: "--expect-target is required"},
		{
			name: "a valid-looking label that is part of the password", url: "postgres://u:pw@SECRETLABEL/app?sslmode=verify-full",
			reject: true, contains: "does not match --expect-target 127.0.0.1:15432", leaks: []string{"SECRETLABEL"},
		},
		{
			name: "the same with plaintext allowed", url: "postgres://u:pw@SECRETLABEL/app?sslmode=disable", policy: connPolicy{allowPlaintext: true},
			reject: true, contains: "does not match --expect-target 127.0.0.1:15432", leaks: []string{"SECRETLABEL"},
		},

		// Not a URL pgx accepts: no pgx text, which can quote the string.
		{name: "unparsable", url: "postgres://u:" + guardPassword + "@h:notaport/app", reject: true, contains: "not a valid connection string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			policy := tc.policy
			if policy.expectTarget == "" && !tc.noTarget {
				policy.expectTarget = "127.0.0.1:15432"
			}
			err := checkBootstrapConn(tc.url, policy)
			if !tc.reject {
				if err != nil {
					t.Fatalf("checkBootstrapConn = %v, want nil", err)
				}

				return
			}
			if err == nil {
				t.Fatal("checkBootstrapConn = nil, want a rejection")
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error %q lacks %q", err, tc.contains)
			}
			pw := tc.password
			if pw == "" {
				pw = guardPassword
			}
			for _, leak := range append([]string{pw, "postgres://", "127.0.0.1", "u:"}, tc.leaks...) {
				if leak == "127.0.0.1" && policy.expectTarget != "" {
					continue // the operator's own --expect-target is fine to print
				}
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error carries %q from the URL or the environment: %q", leak, err)
				}
			}
		})
	}
}

// The check runs in the command before anything connects. The URL points at a
// closed port, so a refused guard is told apart from a failed connection by
// the error alone.
func TestBootstrapCommand_GuardRunsBeforeConnecting(t *testing.T) {
	cfg := &config.AppConfig{
		Postgres: config.Postgres{URL: "postgres://u:" + guardPassword + "@127.0.0.1:1/app?sslmode=disable"},
		Log:      config.Log{Level: "error", Format: "text"},
		Auth:     config.Auth{AdminRole: "admin"},
	}
	run := func(args ...string) error {
		cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"bootstrap"}, args...))

		return cmd.ExecuteContext(context.Background())
	}

	err := run("--expect-target", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "refusing to connect: TLS is disabled") {
		t.Fatalf("plaintext URL by default = %v, want a refusal before connecting", err)
	}
	if strings.Contains(err.Error(), guardPassword) {
		t.Fatalf("the refusal carries the password: %q", err)
	}

	err = run("--expect-target", "127.0.0.1:15432", "--insecure-allow-plaintext")
	if err == nil || !strings.Contains(err.Error(), "does not match --expect-target 127.0.0.1:15432") {
		t.Fatalf("wrong target with the plaintext flag = %v, want a refusal before connecting", err)
	}

	err = run("--expect-target", "127.0.0.1:1", "--insecure-allow-plaintext")
	if err == nil || strings.Contains(err.Error(), "refusing") {
		t.Fatalf("plaintext allowed = %v, want it past the guard (and failing to connect to the closed port)", err)
	}

	err = run("--expect-target", "not-a-target")
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("malformed --expect-target = %v, want a usage error", err)
	}
}

// Whatever the command prints or logs on a refusal — its error, stderr, stdout,
// the default slog output — carries no piece of the URL: not the password and,
// for a password with an unencoded @, not the "host" pgx cuts out of it.
func TestBootstrapCommand_RefusalLeaksNothingFromTheURL(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "SYNTHETIC_SECRET"
	cfg := &config.AppConfig{
		Postgres: config.Postgres{URL: "postgres://u:@" + secret + "@127.0.0.1:15432/app?sslmode=verify-full"},
		Log:      config.Log{Level: "debug", Format: "text"},
		Auth:     config.Auth{AdminRole: "admin"},
	}
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"bootstrap", "--expect-target", "127.0.0.1:15432"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to connect") {
		t.Fatalf("malformed URL = %v, want a refusal before connecting", err)
	}
	for name, out := range map[string]string{"error": err.Error(), "stdout": stdout.String(), "stderr": stderr.String(), "log": logs.String()} {
		if strings.Contains(out, secret) {
			t.Errorf("%s carries a piece of the URL: %q", name, out)
		}
	}
}

// --expect-target is required: without it the command refuses before it reads
// the config or parses the URL, with a fixed message. The URL carries a
// piece-of-the-password host that is a valid label (SECRETLABEL) and an
// unencoded @ (SYNTHETIC_SECRET), and the command is driven the way main
// drives it, with what main would log captured.
func TestBootstrapCommand_ExpectTargetIsRequired(t *testing.T) {
	for _, args := range [][]string{
		{"bootstrap"},
		{"bootstrap", "--expect-target", ""},
		{"bootstrap", "--insecure-allow-plaintext"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			loaded := false
			cfg := &config.AppConfig{
				Postgres: config.Postgres{URL: "postgres://u:@SYNTHETIC_SECRET@SECRETLABEL/app?sslmode=verify-full"},
				Log:      config.Log{Level: "debug", Format: "text"},
				Auth:     config.Auth{AdminRole: "admin"},
			}
			var stdout, stderr bytes.Buffer
			cmd := newRootCmd(func() (*config.AppConfig, error) { loaded = true; return cfg, nil })
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(args)

			err := cmd.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), "--expect-target is required") {
				t.Fatalf("error = %v, want %q", err, "--expect-target is required")
			}
			if loaded {
				t.Error("the config was loaded before the flag was checked")
			}
			slog.Error("worker exited", "error", err) // what main does with it
			for name, out := range map[string]string{"error": err.Error(), "stdout": stdout.String(), "stderr": stderr.String(), "log": logs.String()} {
				for _, frag := range []string{"SYNTHETIC_SECRET", "SECRETLABEL"} {
					if strings.Contains(out, frag) {
						t.Errorf("%s carries %q from the URL: %q", name, frag, out)
					}
				}
			}
		})
	}
}

// With the flag, a host that is a valid label but part of the password is a
// target mismatch, refused before the resolver sees it.
func TestBootstrapCommand_SecretLabelHostIsRefused(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &config.AppConfig{
		Postgres: config.Postgres{URL: "postgres://u:pw@SECRETLABEL/app?sslmode=verify-full"},
		Log:      config.Log{Level: "debug", Format: "text"},
		Auth:     config.Auth{AdminRole: "admin"},
	}
	var stdout, stderr bytes.Buffer
	cmd := newRootCmd(func() (*config.AppConfig, error) { return cfg, nil })
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"bootstrap", "--expect-target", "127.0.0.1:15432"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to connect: connection target does not match --expect-target 127.0.0.1:15432") {
		t.Fatalf("error = %v, want a target mismatch before connecting (not a resolver error)", err)
	}
	slog.Error("worker exited", "error", err) // what main does with it
	for name, out := range map[string]string{"error": err.Error(), "stdout": stdout.String(), "stderr": stderr.String(), "log": logs.String()} {
		if strings.Contains(out, "SECRETLABEL") {
			t.Errorf("%s carries the host: %q", name, out)
		}
	}
}
