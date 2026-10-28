package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

const guardPassword = "s3cretPW"

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
