// Package postgres owns what every bounded context shares about Postgres: the
// gorm handle (pgx underneath) and the migration runner. Row models, queries
// and error translation stay in each BC's infra/postgres.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

	// One parse, whatever the settings: the config that passes the check (when
	// expect_target is set) is the one the pool dials with, and the zone
	// TIMESTAMP columns decode in comes from the same parse, not from a regexp
	// over the raw string (see openPool). Handing gorm the URL would parse it
	// a second time, and a service file or the environment can change between
	// the two reads.
	cc, err := parseConn(cfg)
	if err != nil {
		return nil, nil, err
	}
	pool := openPool(cc, cfg.URL)

	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		_ = pool.Close()

		return nil, nil, fmt.Errorf("open postgres: %w", redactConnErr(err, cc))
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
		return nil, nil, fmt.Errorf("ping postgres: %w", redactConnErr(err, cc))
	}

	return db, cleanup, nil
}

// parseConn parses cfg.URL once. With cfg.ExpectTarget it also checks the
// target (and returns the guard's error naming it); without, it only parses.
// Neither error carries anything parsed from the URL: pgx's own parse error can
// quote the string.
func parseConn(cfg config.Postgres) (*pgx.ConnConfig, error) {
	if cfg.ExpectTarget != "" {
		cc, err := ParseCheckedConn(cfg.URL, ConnPolicy{ExpectTarget: cfg.ExpectTarget, AllowPlaintext: true})
		if err != nil {
			return nil, fmt.Errorf("postgres.expect_target: %w", err)
		}

		return cc, nil
	}
	cc, err := pgx.ParseConfig(cfg.URL)
	if err != nil {
		return nil, errors.New("open postgres: postgres.url is not a valid connection string")
	}

	return cc, nil
}

// timeZoneMatcher is gorm.io/driver/postgres v1.6.3's own (postgres.go:38):
// the TimeZone it looks for in the DSN text.
var timeZoneMatcher = regexp.MustCompile("(time_zone|TimeZone|timezone)=(.*?)($|&| )")

// sessionZone is the zone the driver would install for dsn, or "" for none.
//
// gorm's driver runs timeZoneMatcher over the whole raw string, so a password
// that contains "TimeZone=x" (pTimeZone=x) is taken for a timezone and sent to
// the server as one, which rejects the login and echoes the fragment. Here a
// match counts only when the parsed config agrees that a timezone was set: it
// must have a timezone runtime parameter, and a match equal to its value wins.
// A password never lands in RuntimeParams, so its fragment is ignored. With a
// parameter present but written in a form the matcher reads differently (a
// percent-encoded slash), the first match is kept, so the driver's refusal of
// that zone is kept too. A timezone that comes only from PGTZ or a service
// file has no text in dsn to match, as before: the session gets it, the
// decoding does not.
func sessionZone(cc *pgx.ConnConfig, dsn string) string {
	var param []string
	for k, v := range cc.RuntimeParams {
		if strings.EqualFold(k, "timezone") || strings.EqualFold(k, "time_zone") {
			param = append(param, v)
		}
	}
	if len(param) == 0 {
		return ""
	}
	matches := timeZoneMatcher.FindAllStringSubmatch(dsn, -1)
	for _, m := range matches {
		for _, v := range param {
			if m[2] == v {
				return m[2]
			}
		}
	}
	if len(matches) > 0 {
		return matches[0][2]
	}

	return ""
}

// openPool opens the pool from cc and does what gorm's driver does for a DSN
// (postgres.go:95-121, v1.6.3), which handing the driver a Conn skips: a
// TimeZone in the DSN becomes the session's timezone and the zone TIMESTAMP
// (without time zone) columns decode in. What is dialed comes from cc alone.
// PreferSimpleProtocol and the other driver options are not used by NewDB.
func openPool(cc *pgx.ConnConfig, dsn string) *sql.DB {
	var opts []stdlib.OptionOpenDB
	if zone := sessionZone(cc, dsn); zone != "" {
		cc.RuntimeParams["timezone"] = zone
		opts = append(opts, stdlib.OptionAfterConnect(func(_ context.Context, conn *pgx.Conn) error {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				return err
			}
			conn.TypeMap().RegisterType(&pgtype.Type{
				Name:  "timestamp",
				OID:   pgtype.TimestampOID,
				Codec: &pgtype.TimestampCodec{ScanLocation: loc},
			})

			return nil
		}))
	}

	return stdlib.OpenDB(*cc, opts...)
}
