// Package postgres holds the Postgres repositories of the inventory bounded
// context. Row models and SQL live here and nowhere else; driver errors are
// translated to domain errors before they leave this package.
//
// Migrations are embedded so apps/migrate can run them without the source tree
// (embed cannot cross module boundaries under go.work, so each BC owns its own).
package postgres

import (
	"embed"
	"errors"
	"io/fs"

	"gorm.io/gorm"

	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// Schema is the Postgres schema every table of this BC lives in.
const Schema = "inventory"

//go:embed migrations/*.sql
var migrations embed.FS

// Migration is what apps/migrate registers for this BC.
func Migration() platformpg.Migration {
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err) // embedded directory name is a compile-time constant
	}

	return platformpg.Migration{Schema: Schema, Files: files}
}

// translate maps driver errors onto the given domain errors so callers only
// ever see domain errors (checked with errors.Is) or context errors. Anything
// else is an infrastructure failure and stays wrapped for logging. gorm's
// TranslateError (platform/postgres.NewDB) has already turned SQLSTATE 23505
// into gorm.ErrDuplicatedKey.
func translate(err, notFound, duplicate error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return notFound
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return duplicate
	default:
		return err
	}
}
