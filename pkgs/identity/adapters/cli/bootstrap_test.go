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
