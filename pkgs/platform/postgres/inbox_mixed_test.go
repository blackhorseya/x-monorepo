package postgres_test

import (
	"context"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Message ids are opaque strings: a v4 id from before the v7 switch and a v7
// id from after it dedupe the same way, and neither collides with the other.
// A redelivery (same id) is a duplicate whichever generation minted it.
func TestInbox_MixedV4V7(t *testing.T) {
	c := context.Background()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(telCleanup)
	db, cleanup, err := postgres.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup) // registered first, so it runs last: after the DROP below
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS inboxmixed CASCADE", "CREATE SCHEMA inboxmixed",
		"CREATE TABLE inboxmixed.inbox (message_id VARCHAR(36) PRIMARY KEY, consumed_at TIMESTAMPTZ NOT NULL DEFAULT now())",
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = db.WithContext(context.Background()).Exec("DROP SCHEMA IF EXISTS inboxmixed CASCADE").Error
	})
	inbox := postgres.NewInbox(db, "inboxmixed")

	v7, err := idgen.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"6ba7b810-9dad-41d1-80b4-00c04fd430c8", v7} { // a v4, then a v7
		if first, err := inbox.Record(c, id); err != nil || !first {
			t.Fatalf("first delivery of %s: first=%v err=%v", id, first, err)
		}
	}
	for _, id := range []string{"6ba7b810-9dad-41d1-80b4-00c04fd430c8", v7} {
		if first, err := inbox.Record(c, id); err != nil || first {
			t.Fatalf("redelivery of %s: first=%v err=%v, want a duplicate", id, first, err)
		}
	}
}
