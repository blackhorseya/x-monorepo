package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
)

// slowLock is a census whose lock takes a while to get: before delegating it
// runs wait, which stands for the time spent queued behind another writer (the
// test uses it to move the clock). Whoever decided what "now" is before asking
// for the lock judges key expiry by the instant the wait began.
type slowLock struct {
	port.AdminCensus
	wait func()
}

func (s slowLock) LockAdmins(c context.Context, role string, now time.Time) ([]account.AdminStanding, error) {
	s.wait()

	return s.AdminCensus.LockAdmins(c, role, now)
}

// expiringKey gives the account a key that expires an hour after the fixture's
// current time.
func (f *fixture) expiringKey(t *testing.T, accountID string) string {
	t.Helper()
	k, err := f.createKey.Execute(context.Background(), dto.CreateAPIKeyInput{AccountID: accountID, Name: "short", ExpiresAt: f.clock.t.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	return k.Key.ID
}

// A key that expires while the writer waits for the admin lock must not be
// counted: "usable" is judged at the instant the lock is held.
func TestLastAdmin_KeyExpiresWhileWaitingForTheLock(t *testing.T) {
	c := context.Background()

	t.Run("disable: the backup admin's only key expires", func(t *testing.T) {
		f := newFixture()
		a := f.newAdmin(t, "a", 1)
		b := f.newAdmin(t, "b", 0)
		f.expiringKey(t, b.id)
		census := slowLock{AdminCensus: f.census, wait: func() { f.clock.t = f.clock.t.Add(2 * time.Hour) }}
		disable := usecase.NewDisableAccount(f.accounts, census, adminRole, f.clock)

		if _, err := disable.Execute(c, dto.DisableAccountInput{ID: a.id}); !errors.Is(err, account.ErrLastAdmin) {
			t.Fatalf("err = %v, want ErrLastAdmin", err)
		}
		f.mustStatus(t, a.id, "ACTIVE")
	})

	t.Run("assign roles: the backup admin's only key expires", func(t *testing.T) {
		f := newFixture()
		a := f.newAdmin(t, "a", 1)
		b := f.newAdmin(t, "b", 0)
		f.expiringKey(t, b.id)
		census := slowLock{AdminCensus: f.census, wait: func() { f.clock.t = f.clock.t.Add(2 * time.Hour) }}
		assign := usecase.NewAssignRoles(f.accounts, census, adminRole, f.clock)

		if _, err := assign.Execute(c, dto.AssignRolesInput{ID: a.id, Roles: []string{"viewer"}}); !errors.Is(err, account.ErrLastAdmin) {
			t.Fatalf("err = %v, want ErrLastAdmin", err)
		}
	})

	t.Run("revoke: the backup admin's only key expires", func(t *testing.T) {
		f := newFixture()
		a := f.newAdmin(t, "a", 1)
		b := f.newAdmin(t, "b", 0)
		f.expiringKey(t, b.id)
		census := slowLock{AdminCensus: f.census, wait: func() { f.clock.t = f.clock.t.Add(2 * time.Hour) }}
		revoke := usecase.NewRevokeAPIKey(f.keys, census, adminRole, f.clock)

		if _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[0]}); !errors.Is(err, account.ErrLastAdmin) {
			t.Fatalf("err = %v, want ErrLastAdmin", err)
		}
	})

	t.Run("revoke: the other key of the same admin expires", func(t *testing.T) {
		f := newFixture()
		a := f.newAdmin(t, "a", 1)
		f.expiringKey(t, a.id)
		census := slowLock{AdminCensus: f.census, wait: func() { f.clock.t = f.clock.t.Add(2 * time.Hour) }}
		revoke := usecase.NewRevokeAPIKey(f.keys, census, adminRole, f.clock)

		if _, err := revoke.Execute(c, dto.RevokeAPIKeyInput{ID: a.keys[0]}); !errors.Is(err, account.ErrLastAdmin) {
			t.Fatalf("err = %v, want ErrLastAdmin", err)
		}
	})
}
