package postgres_test

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// dropInboxMixed is the fixture's purge. It returns the error so the caller
// can fail the test: a purge that cannot run must not look like a clean one.
func dropInboxMixed(db *gorm.DB) error {
	return db.WithContext(context.Background()).Exec("DROP SCHEMA IF EXISTS inboxmixed CASCADE").Error
}

// Fault injection: with the pool already closed (the order the earlier
// defer-based cleanup ran in) the purge must report an error, not succeed
// silently and leave the schema behind.
func TestInboxMixedPurge_ReportsAClosedPool(t *testing.T) {
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, config.Postgres{URL: pgtest.URL(t)}, tel)
	if err := dropInboxMixed(db); err != nil { // positive control: an open pool purges fine
		t.Fatalf("purge on an open pool: %v", err)
	}
	pgtest.ClosePool(t, db)
	if err := dropInboxMixed(db); err == nil {
		t.Fatal("purge on a closed pool returned nil: a failed cleanup would pass silently")
	}
}

// Message ids are opaque strings: a v4 id from before the v7 switch and a v7
// id from after it dedupe the same way, and neither collides with the other.
// A redelivery (same id) is a duplicate whichever generation minted it.
func TestInbox_MixedV4V7(t *testing.T) {
	c := context.Background()
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, config.Postgres{URL: pgtest.URL(t)}, tel) // closed by a t.Cleanup registered here, before any purge below: LIFO runs the purge while the pool is open
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS inboxmixed CASCADE", "CREATE SCHEMA inboxmixed",
		"CREATE TABLE inboxmixed.inbox (message_id VARCHAR(36) PRIMARY KEY, consumed_at TIMESTAMPTZ NOT NULL DEFAULT now())",
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := dropInboxMixed(db); err != nil {
			t.Errorf("cleanup: drop schema inboxmixed: %v", err)
		}
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
