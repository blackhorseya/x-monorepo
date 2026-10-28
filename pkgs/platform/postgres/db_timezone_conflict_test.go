//go:build unix

package postgres_test

import (
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// Spellings of TimeZone that name different zones have no right answer: pgx
// sends them in a map and Postgres applies whichever it reads last, so the
// session zone would vary from one connection to the next while TIMESTAMP
// columns decode in just one. NewDB refuses such a URL before it connects, in
// words that carry neither the URL nor the zones.
func TestNewDB_ConflictingTimeZoneKeysAreRefused(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	for _, query := range []string{
		"TimeZone=UTC&timezone=Asia/Taipei",
		"TIMEZONE=UTC&TimeZone=Asia/Taipei",
		"timezone=Asia/Taipei&TimeZone=UTC",
	} {
		for _, pinned := range []bool{false, true} {
			t.Run(query, func(t *testing.T) {
				addr, dials := dialCounter(t)
				cfg := config.Postgres{URL: "postgres://leakyuser:" + secretMarker + "@" + addr + "/leakydb?sslmode=disable&" + query}
				if pinned {
					cfg.ExpectTarget = addr
				}
				_, cleanup, err := postgres.NewDB(cfg, tel)
				if err == nil {
					cleanup()
					t.Fatal("NewDB = nil, want a refusal")
				}
				if !strings.Contains(err.Error(), "postgres.url has conflicting timezone parameters") {
					t.Fatalf("error = %q, want the conflict refusal", err)
				}
				assertNoURLText(t, err, "leakyuser", secretMarker, "leakydb", "Taipei", "UTC")
				if n := dials.Load(); n != 0 {
					t.Fatalf("dials = %d, want 0", n)
				}
			})
		}
	}
}
