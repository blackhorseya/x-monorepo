package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// No database: a worker with handlers must refuse to start (the subscribers would dereference a nil
// *gorm.DB); one without any consumer is idle and starts. Both go through NewWorker, the entry the
// binary calls once its configuration is final, for either broker.
func TestNewWorker_NoDatabase(t *testing.T) {
	tel := pgtest.Telemetry(t)
	one := []Consumer{{"a", make([]eventbus.Subscription, 1)}}

	for _, broker := range []string{config.BrokerPostgres, ""} {
		_, err := NewWorker(config.Worker{Broker: broker}, nil, nil, slog.Default(), tel, nil, one)
		if err == nil || !strings.Contains(err.Error(), "postgres.url is empty") {
			t.Fatalf("NewWorker(broker=%q, nil db, one consumer) = %v, want an error naming postgres.url", broker, err)
		}
	}

	w, err := NewWorker(config.Worker{Broker: config.BrokerPostgres}, nil, nil, slog.Default(), tel, nil, nil)
	if err != nil {
		t.Fatalf("an idle worker (no consumer) needs no database: %v", err)
	}
	if !w.idle {
		t.Fatal("a worker with no consumer must be idle")
	}
}
