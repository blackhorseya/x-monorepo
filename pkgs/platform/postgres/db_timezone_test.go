package postgres_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// Integration (pgtest): postgres.expect_target only checks where NewDB dials;
// it must not change what a row decodes to. gorm's driver, given a DSN with a
// TimeZone, decodes TIMESTAMP (without time zone) columns in that zone; a pool
// opened from an already parsed config has to keep doing so, or every
// timestamp the repositories read shifts by the zone's offset.
func TestNewDB_ExpectTargetKeepsTimestampDecoding(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()

	base := pgtest.URL(t)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	dsn := base + sep + "TimeZone=Asia/Taipei"
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}

	read := func(expectTarget string) time.Time {
		t.Helper()
		db, cleanup, err := postgres.NewDB(config.Postgres{URL: dsn, ExpectTarget: expectTarget}, tel)
		if err != nil {
			t.Fatalf("NewDB(expect_target=%q): %v", expectTarget, err)
		}
		defer cleanup()
		var got time.Time
		if err := db.Raw("SELECT TIMESTAMP '2026-10-28 12:00:00'").Scan(&got).Error; err != nil {
			t.Fatal(err)
		}

		return got
	}

	want := read("")
	if got := read(u.Host); !got.Equal(want) {
		t.Fatalf("with expect_target a TIMESTAMP reads as %s, without it %s: the check changed decoding", got.UTC(), want.UTC())
	}
	if _, off := want.Zone(); off != 8*3600 {
		t.Fatalf("baseline zone offset = %ds, want 28800 (TimeZone=Asia/Taipei is the driver's)", off)
	}
}

// The zone TIMESTAMP columns decode in follows gorm's driver for every way a
// TimeZone can be written, now that NewDB reads it from the parsed config
// instead of leaving it to the driver's regexp over the raw string.
func TestNewDB_TimestampZoneMatrix(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	base := pgtest.URL(t)
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}

	cases := []struct {
		name   string
		query  string
		env    string
		offset int    // seconds east of UTC the decoded TIMESTAMP carries
		reject string // set: NewDB must fail, with this in the error
	}{
		{name: "none", offset: 0},
		{name: "TimeZone", query: "TimeZone=Asia/Taipei", offset: 8 * 3600},
		{name: "lowercase timezone", query: "timezone=Asia/Taipei", offset: 8 * 3600},
		{name: "time_zone is not a Postgres parameter, refused as before", query: "time_zone=Asia/Taipei", reject: "SQLSTATE 42704"},
		{name: "UTC", query: "TimeZone=UTC", offset: 0},
		{name: "after another parameter", query: "application_name=x&TimeZone=Asia/Taipei", offset: 8 * 3600},
		{name: "PGTZ is the session's only, decoding stays UTC", env: "Asia/Taipei", offset: 0},
		{name: "percent-encoded slash is refused as before", query: "TimeZone=Asia%2FTaipei", reject: "connection failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PGTZ", tc.env)
			}
			dsn := base
			if tc.query != "" {
				dsn += sep + tc.query
			}
			db, cleanup, err := postgres.NewDB(config.Postgres{URL: dsn}, tel)
			if tc.reject != "" {
				if err == nil {
					cleanup()
					t.Fatal("NewDB = nil, want a refusal")
				}
				if !strings.Contains(err.Error(), tc.reject) {
					t.Fatalf("error = %q, want %q", err, tc.reject)
				}

				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			var got time.Time
			if err := db.Raw("SELECT TIMESTAMP '2026-10-28 12:00:00'").Scan(&got).Error; err != nil {
				t.Fatal(err)
			}
			if _, off := got.Zone(); off != tc.offset {
				t.Fatalf("decoded %s (offset %ds), want offset %ds", got, off, tc.offset)
			}
		})
	}
}
