//go:build unix

package postgres_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// dialCounter listens on a loopback port and counts the connections it gets,
// closing each at once: the point is where NewDB dials, not what it says.
func dialCounter(t *testing.T) (addr string, dials *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	dials = new(atomic.Int32)
	go func() { // fire-and-forget: ends when the listener closes in Cleanup
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = c.Close()
		}
	}()

	return l.Addr().String(), dials
}

// A service file is state outside the URL: it can change between two reads.
// NewDB must dial what the guard approved, so the URL is parsed once and that
// parse is both checked and used to open the pool. Here the file names
// target A on its first read and, swapped in before that read ends, target B
// on any later one; a guard that parses, then lets the driver parse again,
// approves A and dials B.
func TestNewDB_ExpectTargetHoldsWhenServiceFileChanges(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	addrA, dialsA := dialCounter(t)
	addrB, dialsB := dialCounter(t)
	hostPort := func(addr string) (string, string) {
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatal(err)
		}

		return h, p
	}
	service := func(addr string) string {
		h, p := hostPort(addr)

		return fmt.Sprintf("[probe]\nhost=%s\nport=%s\nsslmode=disable\n", h, p)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "pg_service.conf")
	next := filepath.Join(dir, "next.conf")
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
			// Replace the path before the first read sees EOF, so every later
			// read gets target B.
			err = os.Rename(next, path)
		}
		_ = w.Close()
		writerDone <- err
	}()

	db, cleanup, err := postgres.NewDB(config.Postgres{
		URL:          "service=probe servicefile=" + path + " connect_timeout=1",
		ExpectTarget: addrA,
	}, tel)
	if err == nil {
		cleanup()
		t.Fatalf("NewDB = %v, nil; want an error (the listeners hang up)", db)
	}
	if werr := <-writerDone; werr != nil {
		t.Fatalf("swapping the service file: %v", werr)
	}
	if a, b := dialsA.Load(), dialsB.Load(); a < 1 || b != 0 {
		t.Fatalf("dials A=%d B=%d, want the approved target A dialed and B never (error: %v)", a, b, err)
	}
}
