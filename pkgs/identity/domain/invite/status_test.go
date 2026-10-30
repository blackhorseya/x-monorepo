package invite_test

import (
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
)

func TestStatus(t *testing.T) {
	pending := func() *invite.Invite {
		i, err := invite.New("inv-1", "ops@example.com", "acc-1", now)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	claimed := func() *invite.Invite {
		i := pending()
		if err := i.Claim("ops@example.com", true, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return i
	}

	for name, tc := range map[string]struct {
		i    *invite.Invite
		at   time.Time
		want invite.Status
	}{
		"fresh":                  {pending(), now, invite.StatusPending},
		"last instant":           {pending(), now.Add(invite.TTL - time.Nanosecond), invite.StatusPending},
		"at expiry":              {pending(), now.Add(invite.TTL), invite.StatusExpired},
		"long past":              {pending(), now.Add(30 * 24 * time.Hour), invite.StatusExpired},
		"claimed":                {claimed(), now.Add(2 * time.Hour), invite.StatusClaimed},
		"claimed, then past ttl": {claimed(), now.Add(invite.TTL + time.Hour), invite.StatusClaimed},
	} {
		if got := tc.i.Status(tc.at); got != tc.want {
			t.Errorf("%s: status = %v, want %v", name, got, tc.want)
		}
	}

	for s, want := range map[invite.Status]string{
		invite.StatusPending: "PENDING", invite.StatusClaimed: "CLAIMED", invite.StatusExpired: "EXPIRED", invite.Status(0): "UNKNOWN",
	} {
		if s.String() != want {
			t.Errorf("Status(%d).String() = %q, want %q", s, s.String(), want)
		}
	}
}
