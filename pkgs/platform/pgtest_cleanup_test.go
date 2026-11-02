package platform_test

import (
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/archtest"
)

// A Postgres test must not leave rows behind while reporting PASS: a pool closed
// by a defer before a t.Cleanup purge runs, a close registered after the purge,
// and a purge that discards its error all do (#371; #318, #342, #366, #368).
// pgtest.DB registers the close itself, so the safe order is the only one
// available. CI runs a module's tests only when the change reaches that module,
// so every module with database tests carries this gate; pkgs/platform's
// archtest pins that none is missing.
func TestPostgresTestCleanup(t *testing.T) {
	archtest.RunTestCleanup(t, ".")
}
