package externalidentity_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
)

var now = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func TestNew(t *testing.T) {
	i, err := externalidentity.New("https://idp.example", "sub-1", "acc-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if i.Issuer() != "https://idp.example" || i.Subject() != "sub-1" || i.AccountID() != "acc-1" || !i.CreatedAt().Equal(now) {
		t.Fatalf("identity = %+v", i)
	}
	for name, tc := range map[string]struct {
		iss, sub, acc string
		want          error
	}{
		"empty issuer":  {"", "s", "a", externalidentity.ErrInvalidIssuer},
		"empty subject": {"i", "", "a", externalidentity.ErrInvalidSubject},
		"empty account": {"i", "s", "", externalidentity.ErrInvalidAccount},
	} {
		if _, err := externalidentity.New(tc.iss, tc.sub, tc.acc, now); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestMapping(t *testing.T) {
	for in, want := range map[string]externalidentity.Mapping{
		"":               externalidentity.MappingBound, // the default is fail-closed
		"bound":          externalidentity.MappingBound,
		"auto_provision": externalidentity.MappingAutoProvision,
	} {
		got, err := externalidentity.ParseMapping(in)
		if err != nil || got != want {
			t.Errorf("ParseMapping(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := externalidentity.ParseMapping("auto"); !errors.Is(err, externalidentity.ErrInvalidMapping) {
		t.Fatalf("unknown mapping = %v", err)
	}
	if externalidentity.MappingBound.String() != "bound" || externalidentity.MappingAutoProvision.String() != "auto_provision" || externalidentity.Mapping(0).String() != "unknown" {
		t.Fatal("mapping strings")
	}
}
