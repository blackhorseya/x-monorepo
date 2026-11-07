package main

import (
	"os"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Not pgtest.Main: this package's tests send SIGTERM to their own process to exercise the graceful shutdown, and the
// signal handler Main installs (ctrreap.OnSignal) would turn each into a container kill and exit 143. Without it the
// container is still removed on a normal exit (below) and by the next start after any other end (#481).
func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	os.Exit(code)
}
