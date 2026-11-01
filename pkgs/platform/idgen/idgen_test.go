package idgen_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
)

func TestNew_IsV7(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := idgen.New()
		if err != nil {
			t.Fatal(err)
		}
		u, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("id %q: %v", id, err)
		}
		if u.Version() != 7 || u.Variant() != uuid.RFC4122 {
			t.Fatalf("id %q: version %d variant %v, want v7 RFC 4122", id, u.Version(), u.Variant())
		}
		if len(id) != 36 || id != u.String() {
			t.Fatalf("id %q is not the canonical 36-char form", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
