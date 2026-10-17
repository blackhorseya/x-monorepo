// Package postgres is the Postgres order.Repository of the ordering bounded
// context. Row models and SQL live here and nowhere else; driver errors are
// translated to domain errors before they leave this package.
//
// Migrations are embedded so apps/migrate can run them without the source tree
// (embed cannot cross module boundaries under go.work, so each BC owns its own).
package postgres

import (
	"embed"
	"io/fs"

	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// Schema is the Postgres schema every table of this BC lives in.
const Schema = "ordering"

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
