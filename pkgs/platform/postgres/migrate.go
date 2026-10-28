package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // also registers database/sql driver "pgx"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// Migration is one bounded context's schema: a Postgres schema of its own plus
// the embedded *.sql files (golang-migrate naming: NNNN_name.up.sql / .down.sql).
// Every BC gets its own schema AND its own version table inside that schema
// ("<schema>".schema_migrations), so two BCs sharing a database can never
// overwrite each other's version — the default single public.schema_migrations
// would do exactly that, silently.
type Migration struct {
	Schema string
	Files  fs.FS
}

// Up applies every pending migration. Already current is not an error.
func Up(c context.Context, cfg config.Postgres, m Migration) error {
	return run(c, cfg, m, func(mg *migrate.Migrate) error { return mg.Up() })
}

// Down rolls back one step.
func Down(c context.Context, cfg config.Postgres, m Migration) error {
	return run(c, cfg, m, func(mg *migrate.Migrate) error { return mg.Steps(-1) })
}

// Version reports the current version and whether the last run left it dirty.
// A schema with no migrations applied yet reports (0, false, nil).
func Version(c context.Context, cfg config.Postgres, m Migration) (version uint, dirty bool, err error) {
	err = run(c, cfg, m, func(mg *migrate.Migrate) error {
		v, d, verr := mg.Version()
		if errors.Is(verr, migrate.ErrNilVersion) {
			return nil
		}
		version, dirty = v, d
		return verr
	})

	return version, dirty, err
}

// openMigrateDB opens the handle migrate runs on. With cfg.ExpectTarget set
// the URL is parsed once, checked, and the pool is opened from that very
// config (see NewDB: a second parse could read a changed service file). It is
// the bare pgx path, with none of gorm's driver setup. Unset, the URL goes to
// the "pgx" driver as before.
func openMigrateDB(cfg config.Postgres) (*sql.DB, error) {
	if cfg.ExpectTarget != "" {
		cc, err := ParseCheckedConn(cfg.URL, ConnPolicy{ExpectTarget: cfg.ExpectTarget, AllowPlaintext: true})
		if err != nil {
			return nil, fmt.Errorf("postgres.expect_target: %w", err)
		}

		return stdlib.OpenDB(*cc), nil
	}
	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("open postgres for migrate: %w", redactIfConnect(err, ""))
	}

	return db, nil
}

// run opens a short-lived database/sql handle of its own: golang-migrate's
// Close() closes the *sql.DB it was given, so it must never borrow gorm's pool.
func run(c context.Context, cfg config.Postgres, m Migration, op func(*migrate.Migrate) error) error {
	if m.Schema == "" {
		return errors.New("migration schema must not be empty")
	}
	if cfg.URL == "" {
		return errors.New("postgres url must not be empty")
	}

	db, err := openMigrateDB(cfg)
	if err != nil {
		return err
	}

	// The version table lives inside the BC schema, so the schema has to exist
	// before golang-migrate touches anything.
	if _, err := db.ExecContext(c, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{m.Schema}.Sanitize()); err != nil {
		_ = db.Close()
		return fmt.Errorf("create schema %s: %w", m.Schema, redactIfConnect(err, cfg.ExpectTarget))
	}

	src, err := iofs.New(m.Files, ".")
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("read migrations for %s: %w", m.Schema, err)
	}

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{
		SchemaName:      m.Schema,
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("open migration driver for %s: %w", m.Schema, redactIfConnect(err, cfg.ExpectTarget))
	}

	mg, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		return fmt.Errorf("init migrate for %s: %w", m.Schema, err)
	}
	defer func() { _, _ = mg.Close() }() // closes driver → db

	if err := op(mg); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate %s: %w", m.Schema, redactIfConnect(err, cfg.ExpectTarget))
	}

	return nil
}
