package usecase_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
)

func TestListAccountIdentities(t *testing.T) {
	f := newExtFixture()
	list := usecase.NewListAccountIdentities(f.accounts, f.bindings, f.invites, f.clock)
	c := context.Background()
	acc, other := f.account(t, "admin"), f.account(t)

	invite := func(accountID, email string) {
		t.Helper()
		if _, err := f.createInvite.Execute(c, dto.CreateInviteInput{AccountID: accountID, Email: email}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an account with nothing", func(t *testing.T) {
		out, err := list.Execute(c, dto.ListAccountIdentitiesInput{AccountID: acc.ID})
		if err != nil || len(out.Identities) != 0 || len(out.Invites) != 0 {
			t.Fatalf("out = %+v, %v", out, err)
		}
		// Empty, not nil: the handler maps it without a special case.
		if out.Identities == nil || out.Invites == nil {
			t.Fatalf("nil slices: %+v", out)
		}
	})

	// claimed (then bound), expired, pending, and another account's invite.
	invite(acc.ID, "claimed@example.com")
	if _, err := f.claimInvite.Execute(c, dto.ClaimInviteInput{Issuer: idp, Subject: "sub-1", Email: "claimed@example.com", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	boundAt := f.clock.t
	invite(acc.ID, "expired@example.com")
	f.clock.t = f.clock.t.Add(8 * 24 * time.Hour) // past the 7-day TTL of the one above
	invite(acc.ID, "pending@example.com")
	invite(other.ID, "elsewhere@example.com")

	out, err := list.Execute(c, dto.ListAccountIdentitiesInput{AccountID: acc.ID})
	if err != nil {
		t.Fatal(err)
	}

	if len(out.Identities) != 1 {
		t.Fatalf("identities = %+v", out.Identities)
	}
	if got, want := out.Identities[0], (dto.ExternalIdentityOutput{Issuer: idp, Subject: "sub-1", AccountID: acc.ID, BoundAt: boundAt}); got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}

	// Newest first, each judged at the clock's now.
	var got []string
	for _, v := range out.Invites {
		if v.AccountID != acc.ID {
			t.Fatalf("leaked another account's invite: %+v", v)
		}
		got = append(got, v.Email+"="+v.Status)
	}
	want := []string{"pending@example.com=PENDING", "expired@example.com=EXPIRED", "claimed@example.com=CLAIMED"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("invites = %v, want %v", got, want)
	}
	if v := out.Invites[0]; v.Email != "pending@example.com" || !v.ClaimedAt.IsZero() || !v.ExpiresAt.Equal(f.clock.t.Add(7*24*time.Hour)) {
		t.Fatalf("pending invite = %+v", v)
	}
	if v := out.Invites[2]; v.ClaimedAt.IsZero() {
		t.Fatalf("claimed invite has no claim time: %+v", v)
	}

	t.Run("the other account sees only its own", func(t *testing.T) {
		o, err := list.Execute(c, dto.ListAccountIdentitiesInput{AccountID: other.ID})
		if err != nil || len(o.Identities) != 0 || len(o.Invites) != 1 || o.Invites[0].Email != "elsewhere@example.com" {
			t.Fatalf("other = %+v, %v", o, err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		if _, err := list.Execute(c, dto.ListAccountIdentitiesInput{AccountID: "nope"}); !errors.Is(err, account.ErrNotFound) {
			t.Fatalf("unknown account: %v", err)
		}
		if _, err := list.Execute(c, dto.ListAccountIdentitiesInput{}); !errors.Is(err, account.ErrInvalidID) {
			t.Fatalf("empty account id: %v", err)
		}
	})
}

// The outputs carry no token, hash or secret: an invite has none by design,
// and nothing here may grow one. Checked on the DTO types themselves, so a new
// field that looks like one fails this test before any handler maps it.
func TestListAccountIdentities_OutputsCarryNoSecret(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[dto.ListAccountIdentitiesOutput](),
		reflect.TypeFor[dto.ExternalIdentityOutput](),
		reflect.TypeFor[dto.InviteOutput](),
	} {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			for _, banned := range []string{"token", "hash", "secret"} {
				if strings.Contains(name, banned) {
					t.Errorf("%s.%s looks like a credential", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}
