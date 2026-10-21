package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	os.Exit(code)
}

// Integration (pgtest): the inbox rides the transaction in the context — a
// rolled-back attempt leaves no row, so the retry is a first delivery again.
func TestInbox(t *testing.T) {
	c := context.Background()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	db, cleanup, err := postgres.NewDB(config.Postgres{URL: pgtest.URL(t)}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS inboxtest CASCADE", "CREATE SCHEMA inboxtest",
		"CREATE TABLE inboxtest.inbox (message_id VARCHAR(36) PRIMARY KEY, consumed_at TIMESTAMPTZ NOT NULL DEFAULT now())",
	} {
		if err := db.WithContext(c).Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	inbox := postgres.NewInbox(db, "inboxtest")

	if first, err := inbox.Record(c, "m-1"); err != nil || !first {
		t.Fatalf("first delivery: first=%v err=%v", first, err)
	}
	if first, err := inbox.Record(c, "m-1"); err != nil || first {
		t.Fatalf("redelivery: first=%v err=%v, want a duplicate", first, err)
	}

	// Recorded inside a transaction that fails: the row goes with it.
	boom := errors.New("use case failed")
	err = postgres.Transaction(c, db, func(c context.Context, _ *gorm.DB) error {
		if first, err := inbox.Record(c, "m-2"); err != nil || !first {
			t.Fatalf("in tx: first=%v err=%v", first, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("tx err = %v", err)
	}
	if first, err := inbox.Record(c, "m-2"); err != nil || !first {
		t.Fatalf("after rollback: first=%v err=%v, want a first delivery", first, err)
	}
	var n int64
	if err := db.WithContext(c).Raw("SELECT count(*) FROM inboxtest.inbox").Scan(&n).Error; err != nil || n != 2 {
		t.Fatalf("rows = %d err=%v, want 2", n, err)
	}
}
