// Package postgres owns what every bounded context shares about Postgres: the
// gorm handle (pgx underneath) and the migration runner. Row models, queries
// and error translation stay in each BC's infra/postgres.
package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// NewDB opens gorm for cfg.URL and pings once so misconfiguration fails at
// startup, not on the first request. An empty URL returns a nil *gorm.DB: the
// app has no database and repositories fall back to memory. The cleanup closes
// the pool; wire threads it to the injector's cleanup.
//
// The pool is capped by cfg (MaxOpenConns and friends, set before the first
// connection): every app opens its pool here, so rpc, worker and the CLIs all
// obey postgres.max_open_conns. A config.Postgres built by hand rather than
// config.Load has zeros: unlimited open, database/sql's own idle default (2),
// connections never recycled.
//
// With cfg.ExpectTarget set (postgres.expect_target), the URL is checked
// before anything dials: every connection attempt, fallback hosts included,
// must go to that host:port, or NewDB returns an error naming the expected
// target and nothing else from the URL. The URL is parsed once, and that parse
// opens the pool. Empty = no check. Only the target is
// enforced here; the TLS requirement is `worker bootstrap`'s alone, because
// local development and in-cluster services legitimately run without it.
//
// TranslateError makes the driver surface gorm.ErrDuplicatedKey /
// ErrForeignKeyViolated instead of raw pgconn codes, so BC repositories can
// translate with errors.Is. gorm's own logger is silent: errors already reach
// the usecase Logging decorator, and a second log line per query is noise.
//
// Every statement becomes a child span of the request (see tracing.go).
// Query variables are left out of spans: they are customer data, and the SQL
// shape is what you need to read a trace.
func NewDB(cfg config.Postgres, tel *otel.Providers) (*gorm.DB, func(), error) {
	if cfg.URL == "" {
		return nil, func() {}, nil
	}

	dialector := postgres.Open(cfg.URL)
	var checked *sql.DB // the pool opened from the checked parse, closed if gorm.Open fails
	if cfg.ExpectTarget != "" {
		// Parse once: the config that passes the check is the one the pool
		// dials with. Handing gorm the URL would parse it again, and a service
		// file or the environment can change between the two reads.
		cc, err := ParseCheckedConn(cfg.URL, ConnPolicy{ExpectTarget: cfg.ExpectTarget, AllowPlaintext: true})
		if err != nil {
			return nil, nil, fmt.Errorf("postgres.expect_target: %w", err)
		}
		checked = stdlib.OpenDB(*cc)
		dialector = postgres.New(postgres.Config{Conn: checked})
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		if checked != nil {
			_ = checked.Close()
		}

		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Use(tracing{tracer: tel.Tracer.Tracer("github.com/blackhorseya/x-monorepo/pkgs/platform/postgres")}); err != nil {
		return nil, nil, fmt.Errorf("gorm tracing plugin: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("postgres pool: %w", err)
	}
	cleanup := func() { _ = sqlDB.Close() }
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	if cfg.MaxIdleConns > 0 { // 0 keeps database/sql's default of 2, not "no idle connections"
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(c); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("ping postgres: %w", err)
	}

	return db, cleanup, nil
}
