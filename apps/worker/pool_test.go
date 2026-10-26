package main

import (
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): a pool the subscribers could starve on is refused at
// startup. Five subscribers need 5 x 2 + 1 = 11 connections.
func TestCheckPool(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	bcs := []struct {
		schema string
		subs   []eventbus.Subscription
	}{
		{"a", make([]eventbus.Subscription, 2)},
		{"b", make([]eventbus.Subscription, 3)},
	}

	for _, tc := range []struct {
		name    string
		maxOpen int
		wantErr bool
	}{
		{"unlimited", 0, false},
		{"exactly the floor", 11, false},
		{"default", 25, false},
		{"one short", 10, true},
		{"the pool that deadlocked in practice", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup, err := postgres.NewDB(config.Postgres{URL: pgtest.URL(t), MaxOpenConns: tc.maxOpen}, tel)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			err = checkPool(db, bcs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkPool(max_open=%d) = %v, wantErr %v", tc.maxOpen, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "postgres.max_open_conns") {
				t.Fatalf("error does not name the setting: %v", err)
			}
		})
	}
	if err := checkPool(nil, bcs); err != nil {
		t.Fatalf("no database must pass: %v", err)
	}
}
