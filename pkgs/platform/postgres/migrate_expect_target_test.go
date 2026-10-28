//go:build unix

package postgres_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// migrate opens its own pool, not NewDB's, so postgres.expect_target has to
// be honoured there too: up, down and version all go through the same open.
// The listeners hang up on every connection, so "refused by the guard" and
// "tried to connect" are told apart by the dial count and the error text.
func TestMigrate_ExpectTarget(t *testing.T) {
	m := postgres.Migration{
		Schema: "guard_test",
		Files:  fstest.MapFS{"0001_init.up.sql": {Data: []byte("SELECT 1;")}, "0001_init.down.sql": {Data: []byte("SELECT 1;")}},
	}
	const pw = "s3cretPW"
	ops := map[string]func(cfg config.Postgres) error{
		"up":   func(cfg config.Postgres) error { return postgres.Up(context.Background(), cfg, m) },
		"down": func(cfg config.Postgres) error { return postgres.Down(context.Background(), cfg, m) },
		"version": func(cfg config.Postgres) error {
			_, _, err := postgres.Version(context.Background(), cfg, m)

			return err
		},
	}
	for name, op := range ops {
		t.Run(name+"/mismatch is refused without dialing", func(t *testing.T) {
			addr, dials := dialCounter(t)
			h, p, _ := net.SplitHostPort(addr)
			err := op(config.Postgres{
				URL:          fmt.Sprintf("postgres://u:%s@%s:%s/app?sslmode=disable", pw, h, p),
				ExpectTarget: "127.0.0.1:1",
			})
			if err == nil || !strings.Contains(err.Error(), "postgres.expect_target") {
				t.Fatalf("error = %v, want the guard's refusal", err)
			}
			if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("the error carries the DSN: %q", err)
			}
			if n := dials.Load(); n != 0 {
				t.Fatalf("dials = %d, want 0", n)
			}
		})
		t.Run(name+"/unset connects as before", func(t *testing.T) {
			addr, dials := dialCounter(t)
			h, p, _ := net.SplitHostPort(addr)
			err := op(config.Postgres{URL: fmt.Sprintf("postgres://u:%s@%s:%s/app?sslmode=disable", pw, h, p)})
			if err == nil || strings.Contains(err.Error(), "postgres.expect_target") {
				t.Fatalf("error = %v, want a connection failure, not the guard", err)
			}
			if dials.Load() < 1 {
				t.Fatal("dials = 0, want the connection attempted")
			}
		})
	}
}

// Migrate approves one parse and dials that one: a service file that names A on
// its first read and B on any later one must never reach B.
func TestMigrate_ExpectTargetHoldsWhenServiceFileChanges(t *testing.T) {
	addrA, dialsA := dialCounter(t)
	addrB, dialsB := dialCounter(t)
	service := func(addr string) string {
		h, p, _ := net.SplitHostPort(addr)

		return fmt.Sprintf("[probe]\nhost=%s\nport=%s\nsslmode=disable\n", h, p)
	}
	dir := t.TempDir()
	path, next := filepath.Join(dir, "pg_service.conf"), filepath.Join(dir, "next.conf")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(next, []byte(service(addrB)), 0o600); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() { // fire-and-forget: finishes once the first read of the FIFO ends
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			writerDone <- err

			return
		}
		_, err = w.WriteString(service(addrA))
		if err == nil {
			err = os.Rename(next, path)
		}
		_ = w.Close()
		writerDone <- err
	}()

	m := postgres.Migration{Schema: "guard_test", Files: fstest.MapFS{"0001_init.up.sql": {Data: []byte("SELECT 1;")}}}
	err := postgres.Up(context.Background(), config.Postgres{
		URL:          "service=probe servicefile=" + path + " connect_timeout=1",
		ExpectTarget: addrA,
	}, m)
	if err == nil {
		t.Fatal("Up = nil, want an error (the listeners hang up)")
	}
	if werr := <-writerDone; werr != nil {
		t.Fatalf("swapping the service file: %v", werr)
	}
	if a, b := dialsA.Load(), dialsB.Load(); a < 1 || b != 0 {
		t.Fatalf("dials A=%d B=%d, want the approved target A dialed and B never (error: %v)", a, b, err)
	}
}
