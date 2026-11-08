package main

import (
	"os"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis/redistest"
)

func TestRun_RequiresPostgres(t *testing.T) {
	t.Setenv("APP_POSTGRES_URL", "")
	if err := run(nil); err == nil {
		t.Fatal("missing postgres.url should fail")
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	redistest.Terminate()
	os.Exit(code)
}
