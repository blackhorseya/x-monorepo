package account_test

import (
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
)

func TestAdminStanding(t *testing.T) {
	s := account.NewAdminStanding("acc-1", 2)
	if s.ID() != "acc-1" || s.UsableKeys() != 2 {
		t.Fatalf("standing = %+v", s)
	}
}

func TestGuardLastAdmin(t *testing.T) {
	one := []account.AdminStanding{account.NewAdminStanding("a", 1)}
	two := []account.AdminStanding{account.NewAdminStanding("a", 1), account.NewAdminStanding("b", 3)}

	for name, tc := range map[string]struct {
		admins []account.AdminStanding
		target account.ID
		want   error
	}{
		"the only admin":              {one, "a", account.ErrLastAdmin},
		"the only admin, many keys":   {[]account.AdminStanding{account.NewAdminStanding("a", 5)}, "a", account.ErrLastAdmin},
		"one of two":                  {two, "a", nil},
		"the other of two":            {two, "b", nil},
		"not an effective admin":      {one, "x", nil},
		"no effective admin anywhere": {nil, "x", nil},
	} {
		if err := account.GuardLastAdmin(tc.admins, tc.target); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestGuardLastAdminKey(t *testing.T) {
	for name, tc := range map[string]struct {
		admins []account.AdminStanding
		target account.ID
		want   error
	}{
		"its last key, nobody else":       {[]account.AdminStanding{account.NewAdminStanding("a", 1)}, "a", account.ErrLastAdmin},
		"one of several keys":             {[]account.AdminStanding{account.NewAdminStanding("a", 2)}, "a", nil},
		"its last key, another admin":     {[]account.AdminStanding{account.NewAdminStanding("a", 1), account.NewAdminStanding("b", 1)}, "a", nil},
		"owner is not an effective admin": {[]account.AdminStanding{account.NewAdminStanding("b", 1)}, "a", nil},
		"no effective admin anywhere":     {nil, "a", nil},
	} {
		if err := account.GuardLastAdminKey(tc.admins, tc.target); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}
