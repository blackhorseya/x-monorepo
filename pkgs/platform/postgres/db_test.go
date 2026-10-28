package postgres_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): NewDB applies the pool limits to the sql.DB, and the
// server really never sees more than max_open_conns of them: queries past the
// cap queue for a free connection and all of them still finish.
func TestNewDB_PoolLimits(t *testing.T) {
	const (
		maxOpen = 4
		workers = 16
	)
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	url := pgtest.URL(t)
	// application_name tells this pool's server-side connections apart from the
	// monitor's and from other tests'.
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	db, cleanup, err := postgres.NewDB(config.Postgres{
		URL:             url + sep + "application_name=pooltest",
		MaxOpenConns:    maxOpen,
		MaxIdleConns:    2,
		ConnMaxLifetime: 10 * time.Minute,
		ConnMaxIdleTime: time.Minute,
	}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != maxOpen {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, maxOpen)
	}

	monitor, monitorCleanup, err := postgres.NewDB(config.Postgres{URL: url + sep + "application_name=poolmonitor", MaxOpenConns: 1, MaxIdleConns: 1}, tel)
	if err != nil {
		t.Fatal(err)
	}
	defer monitorCleanup()
	var peak atomic.Int64
	stop := make(chan struct{})
	var monitorDone sync.WaitGroup
	monitorDone.Add(1)
	go func() {
		defer monitorDone.Done()
		for {
			var n int64
			if err := monitor.WithContext(c).Raw("SELECT count(*) FROM pg_stat_activity WHERE application_name = 'pooltest'").Scan(&n).Error; err == nil && n > peak.Load() {
				peak.Store(n)
			}
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	var wg sync.WaitGroup
	var done atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Holds its connection for 200ms: 16 of these through 4 connections take ~800ms.
			if err := db.WithContext(c).Exec("SELECT pg_sleep(0.2)").Error; err != nil {
				t.Errorf("query: %v", err)
				return
			}
			done.Add(1)
		}()
	}
	wg.Wait()
	close(stop)
	monitorDone.Wait()

	if done.Load() != workers {
		t.Fatalf("%d of %d queries finished", done.Load(), workers)
	}
	if p := peak.Load(); p < 2 || p > maxOpen {
		t.Fatalf("server saw at most %d connections of the pool, want between 2 and %d", p, maxOpen)
	}
}

// postgres.expect_target is opt-in: set, NewDB refuses a URL whose target
// differs before it dials, and the error carries the expected host:port only
// (never the DSN). Unset, nothing is checked and the connection is attempted
// as before. The URLs point at a closed port, so "refused by the guard" and
// "tried to connect" are told apart by the error alone.
func TestNewDB_ExpectTarget(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	const pw = "s3cretPW"
	cases := []struct {
		name    string
		url     string
		target  string
		refused bool
	}{
		{name: "unset, no check", url: "postgres://u:" + pw + "@127.0.0.1:1/app?sslmode=disable"},
		{name: "matching target passes the guard", url: "postgres://u:" + pw + "@127.0.0.1:1/app?sslmode=disable", target: "127.0.0.1:1"},
		{name: "other host", url: "postgres://u:" + pw + "@db.internal:5432/app?sslmode=disable", target: "127.0.0.1:1", refused: true},
		{name: "other port", url: "postgres://u:" + pw + "@127.0.0.1:2/app?sslmode=disable", target: "127.0.0.1:1", refused: true},
		{name: "host= override", url: "postgres://u:" + pw + "@127.0.0.1:1/app?sslmode=disable&host=db.internal", target: "127.0.0.1:1", refused: true},
		{name: "fallback host", url: "postgres://u:" + pw + "@127.0.0.1:1,db.internal:5432/app?sslmode=disable", target: "127.0.0.1:1", refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup, err := postgres.NewDB(config.Postgres{URL: tc.url, ExpectTarget: tc.target}, tel)
			if err == nil {
				cleanup()
				t.Fatalf("NewDB = %v, nil; want an error (nothing listens on port 1)", db)
			}
			if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "postgres://") {
				t.Fatalf("the error carries the DSN: %q", err)
			}
			if tc.refused {
				if !strings.Contains(err.Error(), "postgres.expect_target") || strings.Contains(err.Error(), "open postgres") {
					t.Fatalf("error = %q, want the guard's refusal before any connection", err)
				}
				if strings.Contains(err.Error(), "db.internal") {
					t.Fatalf("the error carries the URL's host: %q", err)
				}

				return
			}
			if !strings.Contains(err.Error(), "open postgres") || strings.Contains(err.Error(), "postgres.expect_target") {
				t.Fatalf("error = %q, want it past the guard and failing to connect", err)
			}
		})
	}
}
