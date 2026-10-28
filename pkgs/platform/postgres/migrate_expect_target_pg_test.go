package postgres_test

import (
	"context"
	"net/url"
	"testing"
	"testing/fstest"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): with postgres.expect_target pointing at the real
// server, migrate up, version and down work exactly as without it.
func TestMigrate_ExpectTargetMatchingTargetWorks(t *testing.T) {
	base := pgtest.URL(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	m := postgres.Migration{
		Schema: "expect_target_migrate",
		Files: fstest.MapFS{
			"0001_t.up.sql":   {Data: []byte("CREATE TABLE expect_target_migrate.t (id int);")},
			"0001_t.down.sql": {Data: []byte("DROP TABLE expect_target_migrate.t;")},
		},
	}
	c := context.Background()
	cfg := config.Postgres{URL: base, ExpectTarget: u.Host}
	t.Cleanup(func() { _ = postgres.Down(c, config.Postgres{URL: base}, m) })

	if err := postgres.Up(c, cfg, m); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if v, dirty, err := postgres.Version(c, cfg, m); err != nil || v != 1 || dirty {
		t.Fatalf("Version = %d, %v, %v; want 1, false, nil", v, dirty, err)
	}
	if err := postgres.Down(c, cfg, m); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if v, _, err := postgres.Version(c, cfg, m); err != nil || v != 0 {
		t.Fatalf("Version after Down = %d, %v; want 0, nil", v, err)
	}
}
