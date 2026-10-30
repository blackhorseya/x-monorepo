package cli_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/cli"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

const testSecret = "xk_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

// failWriter fails every write, like a closed pipe under `kubectl logs`.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func bootstrapUC(created bool) cli.BootstrapUC {
	return usecasex.Func[dto.BootstrapInput, *dto.BootstrapOutput](func(context.Context, dto.BootstrapInput) (*dto.BootstrapOutput, error) {
		if !created {
			return &dto.BootstrapOutput{}, nil
		}

		return &dto.BootstrapOutput{
			Created: true,
			Account: dto.AccountOutput{ID: "acc-1", Name: "root", Roles: []string{"admin"}},
			Key:     dto.APIKeyOutput{ID: "key-1", Name: "bootstrap", Prefix: testSecret[:8]},
			Secret:  testSecret,
		}, nil
	})
}

// The transaction has committed by the time the secret is printed, so a
// writer that fails must fail the command — exit 0 would leave an account
// and key whose secret nobody saw. The error says what was created and how
// to recover, and never carries the secret; neither does the log.
func TestBootstrap_WriterErrorIsReturned(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	err := cli.NewBootstrap(bootstrapUC(true)).Run(context.Background(), failWriter{}, "root", []string{"admin"}, "bootstrap", time.Time{})
	if err == nil {
		t.Fatal("Run with a failing writer = nil, want an error")
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error %q does not wrap the writer error", err)
	}
	for _, want := range []string{"key-1", testSecret[:8], "auth.api_keys"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), testSecret[3:]) || strings.Contains(logBuf.String(), testSecret[3:]) {
		t.Fatalf("the secret leaked: err=%q log=%q", err, logBuf.String())
	}
}

func TestBootstrap_AlreadyBootstrappedWriterErrorIsReturned(t *testing.T) {
	err := cli.NewBootstrap(bootstrapUC(false)).Run(context.Background(), failWriter{}, "root", nil, "bootstrap", time.Time{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Run = %v, want the writer error", err)
	}
}

func TestBootstrap_PrintsSecretOnce(t *testing.T) {
	var out bytes.Buffer
	if err := cli.NewBootstrap(bootstrapUC(true)).Run(context.Background(), &out, "root", []string{"admin"}, "bootstrap", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "X-API-Key: "+testSecret) {
		t.Fatalf("output lacks the secret:\n%s", out.String())
	}
}

func inviteUC(created bool) cli.BootstrapUC {
	return usecasex.Func[dto.BootstrapInput, *dto.BootstrapOutput](func(_ context.Context, in dto.BootstrapInput) (*dto.BootstrapOutput, error) {
		if !created {
			return &dto.BootstrapOutput{}, nil
		}

		return &dto.BootstrapOutput{
			Created: true,
			Account: dto.AccountOutput{ID: "acc-1", Name: "root", Roles: []string{"admin"}},
			Invite:  dto.InviteOutput{ID: "inv-1", Email: in.AdminEmail, AccountID: "acc-1", ExpiresAt: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)},
		}, nil
	})
}

// --admin-email prints what the deployer needs to sign in (who, which email,
// until when) and nothing that could be a secret: the invite holds none.
func TestBootstrap_RunInvite(t *testing.T) {
	var buf bytes.Buffer
	if err := cli.NewBootstrap(inviteUC(true)).RunInvite(context.Background(), &buf, "root", []string{"admin"}, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"account acc-1 (root) roles=[admin]", "invite inv-1 for admin@example.com", "expires=2026-11-06T09:00:00Z", "ClaimInvite"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"X-API-Key", "xk_", "secret"} {
		if strings.Contains(got, banned) {
			t.Errorf("output must not mention %q:\n%s", banned, got)
		}
	}

	// Second run: nothing written, exit 0.
	buf.Reset()
	if err := cli.NewBootstrap(inviteUC(false)).RunInvite(context.Background(), &buf, "root", []string{"admin"}, "admin@example.com"); err != nil || !strings.Contains(buf.String(), "already bootstrapped") {
		t.Fatalf("second run = %v\n%s", err, buf.String())
	}

	// A use case error is returned as is.
	boom := errors.New("boom")
	failing := usecasex.Func[dto.BootstrapInput, *dto.BootstrapOutput](func(context.Context, dto.BootstrapInput) (*dto.BootstrapOutput, error) { return nil, boom })
	if err := cli.NewBootstrap(failing).RunInvite(context.Background(), io.Discard, "root", nil, "a@b.co"); !errors.Is(err, boom) {
		t.Fatalf("use case error = %v", err)
	}
	// The invite is committed by the time it is printed: a failed write is an error.
	if err := cli.NewBootstrap(inviteUC(true)).RunInvite(context.Background(), failWriter{}, "root", []string{"admin"}, "a@b.co"); err == nil || !strings.Contains(err.Error(), "inv-1") {
		t.Fatalf("writer failure = %v, want an error naming the invite", err)
	}
}
