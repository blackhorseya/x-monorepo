package system_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/system"
)

func TestGenerators(t *testing.T) {
	c := context.Background()
	id, err := system.NewUUIDGenerator().NewID(c)
	if err != nil {
		t.Fatal(err)
	}
	u, err := uuid.Parse(id)
	if err != nil || len(id) != 36 || u.Version() != 7 {
		t.Fatalf("id = %q, version %d, %v; want a canonical v7", id, u.Version(), err)
	}
	s1, err := system.NewSecretGenerator().NewSecret(c)
	if err != nil || !strings.HasPrefix(s1, system.SecretPrefix) || len(s1) != len(system.SecretPrefix)+43 || len(s1) < apikey.MinSecretLen {
		t.Fatalf("secret = %q, %v", s1, err)
	}
	s2, _ := system.NewSecretGenerator().NewSecret(c)
	if s1 == s2 {
		t.Fatal("two secrets must differ")
	}
	if system.NewClock().Now().Location().String() != "UTC" {
		t.Fatal("clock must be UTC")
	}

	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := system.NewUUIDGenerator().NewID(cancelled); err == nil {
		t.Fatal("cancelled context: want error")
	}
	if _, err := system.NewSecretGenerator().NewSecret(cancelled); err == nil {
		t.Fatal("cancelled context: want error")
	}
}
