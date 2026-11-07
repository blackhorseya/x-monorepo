//go:build unix

package pgtest_test

import (
	"os"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/ctrreap/reaptest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// TestReapFixture is what the harness runs as a child of this very test binary: it starts pgtest's container and holds.
// On its own it skips.
func TestReapFixture(t *testing.T) {
	reaptest.Fixture(t, func(t *testing.T) string { return pgtest.URL(t) })
}

// TestReap: how the container of a test binary ends up when that binary ends in each way a test binary can end (#481).
func TestReap(t *testing.T) { reaptest.Scenarios(t) }
