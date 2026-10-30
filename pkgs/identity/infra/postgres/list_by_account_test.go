package postgres_test

import (
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
)

// ListByAccount of both repositories on a real Postgres: only the account's own
// rows, newest first, and the invite status judged at the clock's now.
func TestListAccountIdentities_OnPostgres(t *testing.T) {
	w := newExtWorld(t)
	w.newAccount(t, "acc-1")
	w.newAccount(t, "acc-2")

	// acc-1: a claimed invite (bound as sub-1), an expired one, a pending one.
	w.invite(t, "acc-1", "claimed@example.com")
	if _, err := w.claimAs("sub-1", "claimed@example.com", true); err != nil {
		t.Fatal(err)
	}
	boundAt := w.clock.Now()
	w.clock.advance(time.Hour) // distinct created_at: the order under test is by time, not by random id
	w.invite(t, "acc-1", "expired@example.com")
	w.clock.advance(8 * 24 * time.Hour)
	w.invite(t, "acc-1", "pending@example.com")
	w.invite(t, "acc-2", "elsewhere@example.com")
	b, err := externalidentity.New(issuer, "sub-elsewhere", "acc-2", w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.bindings.Add(w.ctx, b); err != nil {
		t.Fatal(err)
	}

	list := identity.ProvideListAccountIdentities(w.accounts, w.bindings, w.invites, w.clock, w.rec)
	out, err := list.Execute(w.ctx, dto.ListAccountIdentitiesInput{AccountID: "acc-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Identities) != 1 || out.Identities[0].Subject != "sub-1" || out.Identities[0].Issuer != issuer || !out.Identities[0].BoundAt.Equal(boundAt) {
		t.Fatalf("identities = %+v", out.Identities)
	}
	var got []string
	for _, v := range out.Invites {
		got = append(got, v.Email+"="+v.Status)
	}
	want := []string{"pending@example.com=PENDING", "expired@example.com=EXPIRED", "claimed@example.com=CLAIMED"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("invites = %v, want %v", got, want)
	}
}
