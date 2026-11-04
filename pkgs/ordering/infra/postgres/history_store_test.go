package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func historyDB(t *testing.T) (context.Context, *gorm.DB) {
	t.Helper()
	c := context.Background()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	db := pgtest.DB(t, cfg, pgtest.Telemetry(t))
	if err := platformpg.Up(c, cfg, postgres.Migration()); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(c).Exec(`TRUNCATE ordering.order_history`).Error; err != nil {
		t.Fatal(err)
	}

	return c, db
}

func historyEntry(t *testing.T, id string, at time.Time, kind order.ActorKind, subject string) order.HistoryEntry {
	t.Helper()
	usd, _ := kernel.NewMoney(100, "USD")
	it, _ := order.NewLineItem("S", 1, usd)
	o, err := order.New(order.Params{ID: order.ID(id), CustomerID: "c-1", Items: []order.LineItem{it}, Now: at})
	if err != nil {
		t.Fatal(err)
	}
	h, err := order.NewHistoryEntry(o.PullEvents()[0], kind, subject)
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// The domain clock decides occurred_at (the event's time), not the database's now(); the row
// round-trips every column; rows of one order list by seq.
func TestHistoryStore_RoundTrip(t *testing.T) {
	c, db := historyDB(t)
	s := postgres.NewHistoryStore(db)
	at := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC) // long before now(): a now() default would show

	if err := s.Append(c, historyEntry(t, "o-1", at, order.ActorAdmin, "acc-admin"), historyEntry(t, "o-1", at.Add(time.Hour), order.ActorCustomer, "acc-1")); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(c, "o-1")
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %v, %v", got, err)
	}
	first := got[0]
	if first.Seq() != 1 || first.Event() != "CREATED" || first.From() != order.StatusUnknown || first.To() != order.StatusPending ||
		!first.OccurredAt().Equal(at) || first.ActorKind() != order.ActorAdmin || first.ActorSubject() != "acc-admin" || first.Cause() != order.CauseRPCRequest {
		t.Fatalf("first = %+v", first)
	}
	if got[1].Seq() != 2 || got[1].ActorKind() != order.ActorCustomer || !got[1].OccurredAt().Equal(at.Add(time.Hour)) {
		t.Fatalf("second = %+v", got[1])
	}

	var from *string
	if err := db.WithContext(c).Raw(`SELECT from_status FROM ordering.order_history WHERE order_id = 'o-1' AND seq = 1`).Scan(&from).Error; err != nil {
		t.Fatal(err)
	}
	if from != nil {
		t.Fatalf("from_status of a CREATED row = %q, want NULL", *from)
	}
}

// A transaction that rolls back leaves no row, and the seq it used is free again.
func TestHistoryStore_RollbackLeavesNothing(t *testing.T) {
	c, db := historyDB(t)
	s := postgres.NewHistoryStore(db)
	tx := postgres.NewTransactor(db)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

	boom := errors.New("boom")
	err := tx.Do(c, func(c context.Context) error {
		if err := s.Append(c, historyEntry(t, "o-rb", at, order.ActorSystem, "")); err != nil {
			return err
		}
		inside, err := s.List(c, "o-rb")
		if err != nil || len(inside) != 1 {
			t.Errorf("inside the tx: %v, %v; want the row visible", inside, err)
		}

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Do = %v", err)
	}
	if got, err := s.List(c, "o-rb"); err != nil || len(got) != 0 {
		t.Fatalf("after rollback: %v, %v; want nothing", got, err)
	}
	if err := s.Append(c, historyEntry(t, "o-rb", at, order.ActorSystem, "")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(c, "o-rb"); len(got) != 1 || got[0].Seq() != 1 {
		t.Fatalf("seq after a rolled-back attempt = %+v, want 1", got)
	}
}

// Two transactions appending to one order WITHOUT the orders row lock (which the use cases always
// hold, because Save comes first): the second blocks on the primary key and fails when the first
// commits. That is an infrastructure error, passed up as is — not swallowed, not retried, not turned
// into a domain error, so a consumer would retry the message and a caller would see a 500.
func TestHistoryStore_PrimaryKeyCollisionIsAnInfraError(t *testing.T) {
	c, db := historyDB(t)
	s := postgres.NewHistoryStore(db)
	tx := postgres.NewTransactor(db)
	at := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

	firstIn := make(chan struct{})
	firstGo := make(chan struct{})
	var wg sync.WaitGroup
	var firstErr, secondErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		firstErr = tx.Do(c, func(c context.Context) error {
			if err := s.Append(c, historyEntry(t, "o-pk", at, order.ActorSystem, "")); err != nil {
				return err
			}
			close(firstIn)
			<-firstGo

			return nil
		})
	}()
	go func() {
		defer wg.Done()
		<-firstIn
		secondErr = tx.Do(c, func(c context.Context) error {
			return s.Append(c, historyEntry(t, "o-pk", at, order.ActorSystem, ""))
		})
	}()
	<-firstIn
	time.Sleep(300 * time.Millisecond) // let the second reach its INSERT and block behind the first's uncommitted key
	close(firstGo)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("first writer: %v", firstErr)
	}
	if secondErr == nil {
		t.Fatal("second writer succeeded: the primary key did not stop a duplicate seq")
	}
	var ke *kernel.Error
	if errors.As(secondErr, &ke) {
		t.Fatalf("collision became a domain error %v; it must stay an infrastructure error", secondErr)
	}
	if !strings.Contains(secondErr.Error(), "order_history") && !strings.Contains(secondErr.Error(), "23505") && !strings.Contains(strings.ToLower(secondErr.Error()), "duplicate") {
		t.Fatalf("second writer error = %v, want the database's unique violation", secondErr)
	}
	if got, _ := s.List(c, "o-pk"); len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
}

// Migration 0010 only creates a table: no foreign key to ordering.orders (adding one would lock
// orders for the whole migration transaction, #318), no ALTER of an existing table.
func TestMigration0010_OnlyCreatesTheTable(t *testing.T) {
	c, db := historyDB(t)

	var fks int64
	if err := db.WithContext(c).Raw(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'ordering.order_history'::regclass AND contype = 'f'`).Scan(&fks).Error; err != nil {
		t.Fatal(err)
	}
	if fks != 0 {
		t.Fatalf("order_history has %d foreign keys, want 0", fks)
	}
	var pk string
	if err := db.WithContext(c).Raw(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'ordering.order_history'::regclass AND contype = 'p'`).Scan(&pk).Error; err != nil {
		t.Fatal(err)
	}
	if pk != "PRIMARY KEY (order_id, seq)" {
		t.Fatalf("primary key = %q, want (order_id, seq)", pk)
	}

	files := postgres.Migration().Files
	up, err := fsRead(files, "0010_order_history.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"REFERENCES", "FOREIGN KEY", "ALTER TABLE", "DROP ", "UPDATE ", "INSERT ", "LOCK "} {
		if strings.Contains(strings.ToUpper(stripSQLComments(up)), banned) {
			t.Errorf("0010 up contains %q; it may only CREATE TABLE / INDEX", banned)
		}
	}
	// The way seq is assigned is part of the table's contract and lives next to it.
	if !strings.Contains(up, "MAX(seq)") {
		t.Error("0010 up does not document how seq is assigned (MAX(seq)+1 after the orders Save)")
	}
}

func fsRead(files fs.FS, name string) (string, error) {
	b, err := fs.ReadFile(files, name)

	return string(b), err
}

func stripSQLComments(sql string) string {
	var out []string
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}

	return strings.Join(out, "\n")
}
