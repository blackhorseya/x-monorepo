package system_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/system"
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
	if system.NewClock().Now().Location().String() != "UTC" {
		t.Fatal("clock must be UTC")
	}

	cancelled, cancel := context.WithCancel(c)
	cancel()
	if _, err := system.NewUUIDGenerator().NewID(cancelled); err == nil {
		t.Fatal("cancelled context: want error")
	}
}
