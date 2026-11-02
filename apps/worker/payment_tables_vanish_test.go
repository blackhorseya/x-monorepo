package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"os/signal"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// docs/operations.md, "payment 的表不存在時的 worker", the other direction: payment's tables disappear under a
// worker that is already running (a `migrate down` run while the worker was left up). The worker does not
// exit and does not take the other BCs down with it: the two payment subscribers log an ERROR on every poll
// (watermill's "Error querying for message") and ordering's and inventory's handlers go on; a SIGTERM still
// ends it cleanly. Observed, then pinned here so the runbook's sentence stays true.
func TestWorker_KeepsRunningWhenPaymentTablesVanish(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	url := pgtest.URL(t)
	c := context.Background()
	db := pgtest.DB(t, config.Postgres{URL: url}, pgtest.Telemetry(t))
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration(), paymentpg.Migration()} {
		if err := postgres.Up(c, config.Postgres{URL: url}, m); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("APP_POSTGRES_URL", url)
	t.Setenv("APP_LOG_LEVEL", "error")
	done := make(chan error, 1)
	go func() { done <- run(nil) }()
	time.Sleep(2 * time.Second) // past wiring and into Run
	if err := db.WithContext(c).Exec(`DROP SCHEMA payment CASCADE`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = postgres.Up(context.Background(), config.Postgres{URL: url}, paymentpg.Migration()) })
	select {
	case err := <-done:
		t.Fatalf("the worker exited after the payment schema was dropped: %v", err)
	case <-time.After(4 * time.Second):
		// still running: the subscribers retry, they do not give up
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after SIGTERM = %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no exit on SIGTERM")
	}
}
