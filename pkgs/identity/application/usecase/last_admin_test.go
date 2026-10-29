package usecase_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type adminRef struct {
	id   string
	keys []string
}

// newAdmin makes an ACTIVE account holding the admin role and n keys that never expire.
func (f *fixture) newAdmin(t *testing.T, name string, n int) adminRef {
	t.Helper()
	c := context.Background()
	a, err := f.createAccount.Execute(c, dto.CreateAccountInput{Name: name, Roles: []string{adminRole}})
	if err != nil {
		t.Fatal(err)
	}
	ref := adminRef{id: a.ID}
	for range n {
		k, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: "k"})
		if err != nil {
			t.Fatal(err)
		}
		ref.keys = append(ref.keys, k.Key.ID)
	}

	return ref
}

func (f *fixture) mustStatus(t *testing.T, id, want string) {
	t.Helper()
	got, err := f.getAccount.Execute(context.Background(), dto.GetAccountInput{ID: id})
	if err != nil || got.Status != want {
		t.Fatalf("account %s = %+v, %v; want %s", id, got, err, want)
	}
}

func TestDisableAccount_LastAdmin(t *testing.T) {
	f := newFixture()
	c := context.Background()
	a := f.newAdmin(t, "a", 1)

	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: a.id}); !errors.Is(err, account.ErrLastAdmin) {
		t.Fatalf("disable the only admin: err = %v, want ErrLastAdmin", err)
	}
	f.mustStatus(t, a.id, "ACTIVE")

	b := f.newAdmin(t, "b", 1)
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: a.id}); err != nil {
		t.Fatalf("disable one of two admins: %v", err)
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: b.id}); !errors.Is(err, account.ErrLastAdmin) {
		t.Fatalf("disable the remaining admin: err = %v, want ErrLastAdmin", err)
	}
	f.mustStatus(t, b.id, "ACTIVE")

	// Enable brings the first one back, and then the other may go.
	if _, err := f.enableAccount.Execute(c, dto.EnableAccountInput{ID: a.id}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: b.id}); err != nil {
		t.Fatalf("disable b after a is back: %v", err)
	}
}

func TestAssignRoles_LastAdmin(t *testing.T) {
	f := newFixture()
	c := context.Background()
	a := f.newAdmin(t, "a", 1)

	for name, roles := range map[string][]string{"clear": nil, "other role": {"viewer"}} {
		if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: a.id, Roles: roles}); !errors.Is(err, account.ErrLastAdmin) {
			t.Fatalf("%s: err = %v, want ErrLastAdmin", name, err)
		}
	}
	if got, _ := f.getAccount.Execute(c, dto.GetAccountInput{ID: a.id}); len(got.Roles) != 1 || got.Roles[0] != adminRole {
		t.Fatalf("roles changed despite the refusal: %v", got.Roles)
	}
	// Keeping the admin role next to others is not a loss.
	if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: a.id, Roles: []string{"ops", adminRole}}); err != nil {
		t.Fatalf("keep admin: %v", err)
	}
	b := f.newAdmin(t, "b", 1)
	if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: a.id, Roles: []string{"viewer"}}); err != nil {
		t.Fatalf("drop admin while b remains: %v", err)
	}
	if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: b.id, Roles: nil}); !errors.Is(err, account.ErrLastAdmin) {
		t.Fatalf("drop admin from the remaining one: err = %v, want ErrLastAdmin", err)
	}
}

func TestRevokeAPIKey_LastAdmin(t *testing.T) {
	f := newFixture()
	c := context.Background()
	a := f.newAdmin(t, "a", 2)

	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[0]}); err != nil {
		t.Fatalf("revoke one of two keys: %v", err)
	}
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[1]}); !errors.Is(err, account.ErrLastAdmin) {
		t.Fatalf("revoke the last key of the only admin: err = %v, want ErrLastAdmin", err)
	}
	if list, _ := f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: a.id}); countActive(list.Keys) != 1 {
		t.Fatalf("keys = %+v, want one still active", list.Keys)
	}

	b := f.newAdmin(t, "b", 1)
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[1]}); err != nil {
		t.Fatalf("revoke a's last key while b remains: %v", err)
	}
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: b.keys[0]}); !errors.Is(err, account.ErrLastAdmin) {
		t.Fatalf("revoke b's last key: err = %v, want ErrLastAdmin", err)
	}
}

func countActive(keys []dto.APIKeyOutput) int {
	n := 0
	for _, k := range keys {
		if k.Status == "ACTIVE" {
			n++
		}
	}

	return n
}

// Who counts: only ACTIVE accounts that hold the role and own a key that resolves now.
func TestLastAdmin_WhoCounts(t *testing.T) {
	f := newFixture()
	c := context.Background()
	a := f.newAdmin(t, "a", 1)

	// admin role but no key at all
	keyless := f.newAdmin(t, "keyless", 0)
	// admin role but its only key is revoked
	revoked := f.newAdmin(t, "revoked", 1)
	// (revoke needs another admin standing, which a and revoked itself provide)
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: revoked.keys[0]}); err != nil {
		t.Fatal(err)
	}
	// admin role but disabled
	disabled := f.newAdmin(t, "disabled", 1)
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: disabled.id}); err != nil {
		t.Fatal(err)
	}
	// admin role but its only key has expired
	expiring := f.newAdmin(t, "expiring", 0)
	exp := f.clock.t.Add(time.Hour)
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: expiring.id, Name: "short", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	// a key but no admin role
	viewer, _ := f.createAccount.Execute(c, dto.CreateAccountInput{Name: "viewer", Roles: []string{"viewer"}})
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: viewer.ID, Name: "k"}); err != nil {
		t.Fatal(err)
	}
	f.clock.t = exp.Add(time.Second)

	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: a.id}); !errors.Is(err, account.ErrLastAdmin) {
		t.Fatalf("a is the only effective admin: err = %v, want ErrLastAdmin", err)
	}

	// Taking standing away from accounts that have none changes nothing.
	for _, id := range []string{keyless.id, revoked.id, expiring.id, viewer.ID} {
		if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: id, Roles: []string{"viewer"}}); err != nil {
			t.Fatalf("assign roles on %s: %v", id, err)
		}
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: viewer.ID}); err != nil {
		t.Fatalf("disable a non-admin: %v", err)
	}
	// Revoking a key that no longer resolves does not cost a its standing.
	list, _ := f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: expiring.id})
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: list.Keys[0].ID}); err != nil {
		t.Fatalf("revoke an expired key: %v", err)
	}
}

// A store with no effective admin at all (static auth.api_keys only, or a
// role mismatch at bootstrap) has nothing to protect: nothing is refused.
func TestLastAdmin_NoAdminToProtect(t *testing.T) {
	f := newFixture()
	c := context.Background()
	keyless := f.newAdmin(t, "keyless", 0)

	if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: keyless.id, Roles: nil}); err != nil {
		t.Fatalf("assign roles: %v", err)
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: keyless.id}); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

// rendezvous holds each writer, right after its census, until both have taken
// theirs (or a moment has passed). Writers that are not serialised both see the
// other still standing and both pass; serialised ones never meet, the second
// being held back before its census, so the wait times out for the first.
type rendezvous struct {
	port.AdminCensus
	arrived atomic.Int32
	both    chan struct{}
}

func newRendezvous(inner port.AdminCensus) *rendezvous {
	return &rendezvous{AdminCensus: inner, both: make(chan struct{})}
}

func (r *rendezvous) LockAdmins(c context.Context, role string, now time.Time) ([]account.AdminStanding, error) {
	admins, err := r.AdminCensus.LockAdmins(c, role, now)
	if r.arrived.Add(1) == 2 {
		close(r.both)
	}
	select {
	case <-r.both:
	case <-time.After(150 * time.Millisecond):
	}

	return admins, err
}

// race runs both calls at once and requires that exactly one is refused as the last admin.
func race(t *testing.T, calls ...func() error) {
	t.Helper()
	errs := make([]error, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = call()
		}()
	}
	wg.Wait()

	refused, ok := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, account.ErrLastAdmin):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("results = %v; want exactly one success and one ErrLastAdmin", errs)
	}
}

func TestLastAdmin_Concurrent(t *testing.T) {
	tx := memory.NewTransactor()

	t.Run("two admins disable each other", func(t *testing.T) {
		f := newFixture()
		a, b := f.newAdmin(t, "a", 1), f.newAdmin(t, "b", 1)
		census := newRendezvous(f.census)
		disable := usecasex.Chain[dto.DisableAccountInput, *dto.AccountOutput](
			usecase.NewDisableAccount(f.accounts, census, adminRole, f.clock),
			usecasex.Transactional[dto.DisableAccountInput, *dto.AccountOutput](tx),
		)
		c := context.Background()
		race(t,
			func() error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: a.id}); return err },
			func() error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: b.id}); return err },
		)
	})

	t.Run("two keys of the only admin revoked at once", func(t *testing.T) {
		f := newFixture()
		a := f.newAdmin(t, "a", 2)
		census := newRendezvous(f.census)
		revoke := usecasex.Chain[dto.RevokeAPIKeyInput, *dto.APIKeyOutput](
			usecase.NewRevokeAPIKey(f.keys, census, adminRole, f.clock),
			usecasex.Transactional[dto.RevokeAPIKeyInput, *dto.APIKeyOutput](tx),
		)
		c := context.Background()
		race(t,
			func() error { _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[0]}); return err },
			func() error { _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[1]}); return err },
		)
	})

	t.Run("one admin dropped by role, the other disabled", func(t *testing.T) {
		f := newFixture()
		a, b := f.newAdmin(t, "a", 1), f.newAdmin(t, "b", 1)
		census := newRendezvous(f.census)
		c := context.Background()
		disable := usecasex.Chain[dto.DisableAccountInput, *dto.AccountOutput](
			usecase.NewDisableAccount(f.accounts, census, adminRole, f.clock),
			usecasex.Transactional[dto.DisableAccountInput, *dto.AccountOutput](tx),
		)
		assign := usecasex.Chain[dto.AssignRolesInput, *dto.AccountOutput](
			usecase.NewAssignRoles(f.accounts, census, adminRole, f.clock),
			usecasex.Transactional[dto.AssignRolesInput, *dto.AccountOutput](tx),
		)
		race(t,
			func() error { _, err := disable.Execute(c, dto.DisableAccountInput{ID: a.id}); return err },
			func() error {
				_, err := assign.Execute(c, dto.AssignRolesInput{ID: b.id, Roles: []string{"viewer"}})
				return err
			},
		)
	})
}
