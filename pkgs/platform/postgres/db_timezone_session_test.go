package postgres_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// The zone TIMESTAMP columns decode in is the zone the session was given,
// whichever way pgx got it: from the URL, from PGTZ, from a service file, or
// from a percent-encoded value in the URL. Before, only a TimeZone written out
// in the DSN text installed the codec; the other three reached the session and
// left the decoding in UTC, and a percent-encoded zone was refused outright.
//
// Each case checks the session zone and that LOCALTIMESTAMP decodes to the same
// instant as CURRENT_TIMESTAMP, which is only so when the two agree.
func TestNewDB_DecodesInTheSessionsOwnTimeZone(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	base := pgtest.URL(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}

	serviceFile := filepath.Join(t.TempDir(), "pg_service.conf")
	if err := os.WriteFile(serviceFile, []byte(fmt.Sprintf(
		"[probe]\nhost=%s\nport=%s\ndbname=%s\nuser=%s\npassword=%s\nsslmode=disable\ntimezone=Asia/Taipei\n",
		u.Hostname(), u.Port(), strings.TrimPrefix(u.Path, "/"), u.User.Username(), pw)), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, dsn, pgtz string }{
		{name: "PGTZ only", dsn: base, pgtz: "Asia/Taipei"},
		{name: "timezone from a service file", dsn: "service=probe servicefile=" + serviceFile},
		{name: "percent-encoded zone in the URL", dsn: base + sep + "TimeZone=Asia%2FTaipei"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pgtz != "" {
				t.Setenv("PGTZ", tc.pgtz)
			}
			for i := 0; i < 10; i++ {
				db, cleanup, err := postgres.NewDB(config.Postgres{URL: tc.dsn}, tel)
				if err != nil {
					t.Fatalf("NewDB: %v", err)
				}
				var row struct {
					Actual  time.Time
					Decoded time.Time
					Session string
				}
				err = db.Raw("SELECT CURRENT_TIMESTAMP AS actual, LOCALTIMESTAMP AS decoded, current_setting('TimeZone') AS session").Scan(&row).Error
				cleanup()
				if err != nil {
					t.Fatal(err)
				}
				if row.Session != "Asia/Taipei" {
					t.Fatalf("pool %d: session timezone = %q, want Asia/Taipei", i, row.Session)
				}
				if d := row.Decoded.Sub(row.Actual); d > time.Minute || d < -time.Minute {
					t.Fatalf("pool %d: LOCALTIMESTAMP decodes %s away from CURRENT_TIMESTAMP: session and decoding disagree", i, d)
				}
			}
		})
	}
}
