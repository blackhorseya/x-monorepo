package main

import (
	"os"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestRun_Usage(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("no args should fail")
	}
	t.Setenv("APP_POSTGRES_URL", "")
	if err := run([]string{"up"}); err == nil {
		t.Fatal("missing APP_POSTGRES_URL should fail")
	}
}

// Integration: a throwaway Postgres container, or TEST_DATABASE_URL (see pgtest).
func TestRun_UpVersionDown(t *testing.T) {
	url := pgtest.URL(t)
	t.Setenv("APP_POSTGRES_URL", url)

	for _, cmd := range []string{"up", "up", "version", "down", "up"} { // second up: already current is fine
		if err := run([]string{cmd}); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
	if err := run([]string{"sideways"}); err == nil {
		t.Fatal("unknown command should fail")
	}
}
