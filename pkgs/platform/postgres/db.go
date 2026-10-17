// Package postgres owns what every bounded context shares about Postgres: the
// gorm handle (pgx underneath) and the migration runner. Row models, queries
// and error translation stay in each BC's infra/postgres.
package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// NewDB opens gorm for cfg.URL and pings once so misconfiguration fails at
// startup, not on the first request. An empty URL returns a nil *gorm.DB: the
// app has no database and repositories fall back to memory. The cleanup closes
// the pool; wire threads it to the injector's cleanup.
//
// TranslateError makes the driver surface gorm.ErrDuplicatedKey /
// ErrForeignKeyViolated instead of raw pgconn codes, so BC repositories can
// translate with errors.Is. gorm's own logger is silent: errors already reach
// the usecase Logging decorator, and a second log line per query is noise.
func NewDB(cfg config.Postgres) (*gorm.DB, func(), error) {
	if cfg.URL == "" {
		return nil, func() {}, nil
	}

	db, err := gorm.Open(postgres.Open(cfg.URL), &gorm.Config{
		TranslateError: true,
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("postgres pool: %w", err)
	}
	cleanup := func() { _ = sqlDB.Close() }

	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(c); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("ping postgres: %w", err)
	}

	return db, cleanup, nil
}
