package identity_test

import (
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/archtest"
)

// DDD rules depguard cannot express: private domain state, constructors, no
// setters, no wall clock in domain, use case shape, dto purity, port purity.
func TestArchitecture(t *testing.T) {
	archtest.Run(t, ".")
}
