package main

import (
	"os"
	"testing"
)

func TestRun_Usage(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("no args should fail")
	}
	t.Setenv("APP_POSTGRES_URL", "")
	if err := run([]string{"up"}); err == nil {
		t.Fatal("missing APP_POSTGRES_URL should fail")
	}
}

// Integration: needs TEST_DATABASE_URL (see pkgs/ordering/infra/postgres).
func TestRun_UpVersionDown(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI: a skipped integration test would be a false green")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
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
