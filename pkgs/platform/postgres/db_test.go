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
