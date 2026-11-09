package platform_test

import (
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/archtest"
)

// The shared packages import no bounded context. kernel has no dependencies, so
// its test is run from here (it must not require platform for it).
func TestArchitecture(t *testing.T) {
	for _, dir := range []string{".", "../kernel"} {
		t.Run(dir, func(t *testing.T) { archtest.RunImports(t, dir) })
	}
}
