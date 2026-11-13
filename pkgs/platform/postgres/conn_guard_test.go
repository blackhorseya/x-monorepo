package postgres

import (
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

const guardPassword = "s3cretPW"

// The target check runs on pgx's own parse of the URL — the parse gorm's
// postgres driver does for the real connection — so what it approves is what
// pgx will do, not what a shell or a regexp thinks the URL says. It goes
// through parseConn, the first step of NewDB, with postgres.expect_target set.
// Every rejection names a reason and at most the configured host:port; the URL
// and the password never appear.
func TestParseConn_ExpectTarget(t *testing.T) {
	const (
		target = "127.0.0.1:15432"
		base   = "postgres://u:" + guardPassword + "@127.0.0.1:15432/app"
	)

	cases := []struct {
		name     string
		url      string
		target   string   // postgres.expect_target; defaults to target
		password string   // what must not leak; defaults to guardPassword
		leaks    []string // more fragments of the URL or environment the error must not carry
		env      map[string]string
		reject   bool
		contains string // part of the expected error
	}{
		// What passes.
		{name: "the target", url: base + "?sslmode=disable"},
		{name: "several hosts, every one the target", url: "postgres://u:" + guardPassword + "@127.0.0.1:15432,127.0.0.1:15432/app?sslmode=verify-full"},
		{name: "IPv6 host", url: "postgres://u:" + guardPassword + "@[::1]:5432/app?sslmode=verify-full", target: "[::1]:5432"},
		{name: "DNS name with hyphens and digits", url: "postgres://u:" + guardPassword + "@db-1.eu-west.example.com:5432/app?sslmode=verify-full", target: "db-1.eu-west.example.com:5432"},
		{name: "unix socket directory", url: base + "?host=/var/run/postgresql", target: "/var/run/postgresql:15432"},
		// A fully qualified name ends in a root dot; only that one trailing dot is allowed.
		{name: "localhost with a root dot", url: "postgres://u:" + guardPassword + "@localhost.:5432/app?sslmode=verify-full", target: "localhost.:5432"},
		{name: "FQDN with a root dot", url: "postgres://u:" + guardPassword + "@db.example.com.:5432/app?sslmode=verify-full", target: "db.example.com.:5432"},

		// The target.
		{name: "several hosts, they differ", url: "postgres://u:" + guardPassword + "@127.0.0.1:15432,b.example:5432/app?sslmode=verify-full", reject: true, contains: "connection target does not match 127.0.0.1:15432", leaks: []string{"b.example"}},
		{
			name: "host override in the query", url: base + "?sslmode=verify-full&host=evil.example",
			reject: true, contains: "connection target does not match 127.0.0.1:15432", leaks: []string{"evil.example"},
		},
		{name: "authority host is not the expected target", url: "postgres://u:" + guardPassword + "@db.internal:5432/app?sslmode=verify-full", reject: true, contains: "connection target does not match 127.0.0.1:15432", leaks: []string{"db.internal"}},
		{name: "one of several hosts is not the target", url: "postgres://u:" + guardPassword + "@127.0.0.1:15432,evil.example:5432/app?sslmode=verify-full", reject: true, contains: "connection target does not match 127.0.0.1:15432", leaks: []string{"evil.example"}},
		{name: "the target's port must match too", url: "postgres://u:" + guardPassword + "@127.0.0.1:5432/app?sslmode=verify-full", reject: true, contains: "connection target does not match 127.0.0.1:15432"},
		{
			name: "a valid-looking label that is part of the password", url: "postgres://u:pw@SECRETLABEL/app?sslmode=verify-full",
			reject: true, contains: "connection target does not match 127.0.0.1:15432", leaks: []string{"SECRETLABEL"},
		},

		// A password with an unencoded @: pgx takes the text after the first @ as the
		// host, so the "host" is part of the password. Neither goes into a message.
		{
			name: "unencoded @ in the password, wrong target", url: "postgres://u:@SYNTHETIC_SECRET@127.0.0.1:15432/app?sslmode=verify-full",
			password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host",
		},
		{
			name: "unencoded @ in the password, plaintext", url: "postgres://u:pa@SYNTHETIC_SECRET@127.0.0.1:15432/app?sslmode=disable",
			password: "SYNTHETIC_SECRET", leaks: []string{"pa@"}, reject: true, contains: "invalid host",
		},

		// The host can come from the environment, not the URL.
		{
			name: "PGHOST decides the host", url: "postgres:///app?sslmode=verify-full", env: map[string]string{"PGHOST": "pghost.example"},
			reject: true, contains: "connection target does not match 127.0.0.1:15432", leaks: []string{"pghost.example"},
		},
		{
			name: "PGHOST that is not a host name", url: "postgres:///app?sslmode=verify-full", env: map[string]string{"PGHOST": "SYNTHETIC_PGHOST"},
			reject: true, contains: "invalid host", leaks: []string{"SYNTHETIC_PGHOST"},
		},

		// The host must be an IP, a DNS name or a unix socket directory: a host pgx
		// cut out of the password otherwise reaches the resolver, whose error quotes
		// it into the log.
		{name: "host with a slash", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET/x", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "host with a space", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET%20x", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "host with a percent", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET%2541", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "host with an encoded @", url: base + "?sslmode=verify-full&host=SYNTHETIC_SECRET%40127.0.0.1", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},
		{name: "empty label", url: base + "?sslmode=verify-full&host=a..example", reject: true, contains: "invalid host"},
		{name: "label starts with a hyphen", url: base + "?sslmode=verify-full&host=-a.example", reject: true, contains: "invalid host"},
		{name: "label over 63 chars", url: base + "?sslmode=verify-full&host=" + strings.Repeat("a", 64) + ".example", reject: true, contains: "invalid host"},
		{
			name: "a bad host in a fallback", url: "postgres://u:" + guardPassword + "@a.example:5432,SYNTHETIC_SECRET@b:5432/app?sslmode=verify-full",
			password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host",
		},
		{name: "empty label in the middle", url: base + "?sslmode=verify-full&host=a..b", reject: true, contains: "invalid host"},
		{name: "a lone root dot", url: base + "?sslmode=verify-full&host=.", reject: true, contains: "invalid host"},
		{name: "two root dots", url: base + "?sslmode=verify-full&host=..", reject: true, contains: "invalid host"},
		{name: "two trailing dots", url: base + "?sslmode=verify-full&host=db.example.com..", reject: true, contains: "invalid host"},
		{name: "socket path with a space", url: base + "?host=/var/run/SYNTHETIC_SECRET%20x", password: "SYNTHETIC_SECRET", reject: true, contains: "invalid host"},

		// Not a URL pgx accepts: no pgx text, which can quote the string.
		{name: "unparsable", url: "postgres://u:" + guardPassword + "@h:notaport/app", reject: true, contains: "not a valid connection string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			expect := tc.target
			if expect == "" {
				expect = target
			}
			cc, err := parseConn(config.Postgres{URL: tc.url, ExpectTarget: expect})
			if !tc.reject {
				if err != nil {
					t.Fatalf("parseConn = %v, want nil", err)
				}
				if cc == nil {
					t.Fatal("parseConn = nil config, want the config it approved")
				}

				return
			}
			if err == nil {
				t.Fatal("parseConn = nil, want a rejection")
			}
			if !strings.HasPrefix(err.Error(), "postgres.expect_target: ") || strings.Count(err.Error(), "expect_target") != 1 {
				t.Fatalf("error %q does not name postgres.expect_target exactly once", err)
			}
			if strings.Contains(err.Error(), "--expect-target") {
				t.Fatalf("error %q names a flag that does not exist", err)
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error %q lacks %q", err, tc.contains)
			}
			pw := tc.password
			if pw == "" {
				pw = guardPassword
			}
			for _, leak := range append([]string{pw, "postgres://", "u:"}, tc.leaks...) {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the error carries %q from the URL or the environment: %q", leak, err)
				}
			}
		})
	}
}
