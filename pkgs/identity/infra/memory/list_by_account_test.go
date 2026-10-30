package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/externalidentity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
)

func TestExternalIdentityRepository_ListByAccount(t *testing.T) {
	c := context.Background()
	repo := memory.NewExternalIdentityRepository()

	if got, err := repo.ListByAccount(c, "acc-1"); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for _, b := range []struct {
		iss, sub, acc string
		at            time.Time
	}{
		{"https://idp.example", "old", "acc-1", now},
		{"https://idp.example", "new", "acc-1", now.Add(time.Hour)},
		{"https://other.example", "new", "acc-1", now.Add(time.Hour)},
		{"https://idp.example", "elsewhere", "acc-2", now},
	} {
		e, err := externalidentity.New(b.iss, b.sub, b.acc, b.at)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Add(c, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListByAccount(c, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	// Newest first; the tie on created_at breaks by issuer, then subject.
	var order []string
	for _, e := range got {
		if e.AccountID() != "acc-1" {
			t.Fatalf("leaked another account's binding: %+v", e)
		}
		order = append(order, e.Issuer()+"|"+e.Subject())
	}
	want := []string{"https://idp.example|new", "https://other.example|new", "https://idp.example|old"}
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestInviteRepository_ListByAccount(t *testing.T) {
	c := context.Background()
	repo := memory.NewInviteRepository()

	if got, err := repo.ListByAccount(c, "acc-1"); err != nil || len(got) != 0 {
		t.Fatalf("empty = %v, %v", got, err)
	}
	for _, v := range []struct {
		id, email, acc string
		at             time.Time
	}{
		{"inv-a", "a@example.com", "acc-1", now},
		{"inv-b", "b@example.com", "acc-1", now.Add(time.Hour)},
		{"inv-c", "c@example.com", "acc-1", now.Add(time.Hour)},
		{"inv-x", "x@example.com", "acc-2", now},
	} {
		i, err := invite.New(invite.ID(v.id), v.email, v.acc, v.at)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Add(c, i); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListByAccount(c, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, i := range got {
		if i.AccountID() != "acc-1" {
			t.Fatalf("leaked another account's invite: %v", i.ID())
		}
		ids = append(ids, i.ID().String())
	}
	// Newest first (created_at desc, id desc).
	want := []string{"inv-c", "inv-b", "inv-a"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}
