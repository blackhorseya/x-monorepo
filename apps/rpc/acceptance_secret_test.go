package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/acceptance"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// The database URL carries the password, and test output ends up in terminals and CI logs. These tests call the
// paths that handle the URL with a synthetic password and require it, and the host the URL names, to be absent
// from every message they produce.
const synthSecret = "S3cr3tSynth"

// recorder is a testing.TB that records what the code under test says instead of failing the real test. A Fatalf
// stops the code like the real one (panic, recovered by run).
type recorder struct {
	testing.TB
	out []string
}

type stopped struct{}

func (r *recorder) Fatalf(format string, args ...any) {
	r.out = append(r.out, fmt.Sprintf(format, args...))
	panic(stopped{})
}
func (r *recorder) Logf(format string, args ...any) {
	r.out = append(r.out, fmt.Sprintf(format, args...))
}
func (r *recorder) Errorf(format string, args ...any) {
	r.out = append(r.out, fmt.Sprintf(format, args...))
}

func (r *recorder) run(f func()) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(stopped); !ok {
				panic(v)
			}
		}
	}()
	f()
}

func (r *recorder) text() string { return strings.Join(r.out, "\n") }

func requireNoSecret(t *testing.T, what, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s != "" && strings.Contains(text, s) {
			t.Errorf("%s echoes %q:\n%s", what, s, text)
		}
	}
}

// (1) a URL that does not parse.
func TestSecret_MalformedURL(t *testing.T) {
	_, err := checkBaseURL("postgres://postgres:" + synthSecret + "%ZZ@127.0.0.1:1/postgres")
	if err == nil {
		t.Fatal("want an error for a bad escape")
	}
	requireNoSecret(t, "checkBaseURL", err.Error(), synthSecret, "127.0.0.1", "%ZZ")

	// the other refusals, and the pgx-facing helpers, say a fixed thing too
	for _, raw := range []string{
		"host=127.0.0.1 password=" + synthSecret + " dbname=postgres",
		"mysql://u:" + synthSecret + "@127.0.0.1/x",
		"postgres://u:" + synthSecret + "@127.0.0.1/x?dbname=y",
		"postgres://u:" + synthSecret + "@127.0.0.1/x?sslmode=bogus&options=a",
	} {
		_, err := checkBaseURL(raw)
		if err == nil {
			continue
		}
		requireNoSecret(t, "checkBaseURL("+strings.SplitN(raw, ":", 2)[0]+")", err.Error(), synthSecret, "127.0.0.1")
	}
	// a URL that parses as a URL but not as a pgx config (the sslmode value is invalid): pgx's own error echoes the url
	base, err := checkBaseURL("postgres://u:" + synthSecret + "@127.0.0.1:1/x?sslmode=bogus")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&template{base: base}).urlFor("acc_x")
	if err == nil {
		t.Fatal("urlFor accepted an invalid sslmode")
	}
	requireNoSecret(t, "urlFor", err.Error(), synthSecret, "127.0.0.1")
	err = (&template{live: map[string]bool{}}).drop(context.Background(), "postgres://u:"+synthSecret+"@127.0.0.1:1/x?sslmode=bogus")
	if err == nil {
		t.Fatal("drop accepted an invalid url")
	}
	requireNoSecret(t, "drop", err.Error(), synthSecret, "127.0.0.1")
}

// (2) a well-formed URL that cannot connect (nothing listens on port 1), through pgtest.Conn as the harness does.
func TestSecret_UnreachableDatabase(t *testing.T) {
	r := &recorder{TB: t}
	r.run(func() {
		checkIdentity(r, "postgres://u:"+synthSecret+"@127.0.0.1:1/x?sslmode=disable", "x")
	})
	if len(r.out) == 0 {
		t.Fatal("checkIdentity reported nothing for an unreachable database")
	}
	requireNoSecret(t, "checkIdentity (unreachable)", r.text(), synthSecret)
}

// (3) the normal path: the password of the database this run really uses, in everything the identity check and the
// scenario URLs log or return. (A container's password is fixed by pgtest; TEST_DATABASE_URL brings its own.)
func TestSecret_NormalRun(t *testing.T) {
	tpl := newTemplate(t)
	secret, _ := tpl.base.User.Password()
	if secret == "" {
		t.Skip("the database url has no password to look for")
	}
	r := &recorder{TB: t}
	r.run(func() {
		dbURL := tpl.make()
		checkIdentity(r, dbURL, tpl.name+"-not-this-one") // a mismatch message, the other thing identity says
	})
	requireNoSecret(t, "identity check", r.text(), secret)
	requireNoSecret(t, "template name", tpl.name, secret)
}

// The children's own output is printed when one fails: apps/migrate and apps/worker must not print APP_POSTGRES_URL's
// password, whether the url does not parse or the database cannot be reached.
func TestSecret_ChildOutput(t *testing.T) {
	dir := t.TempDir()
	for _, app := range []string{"migrate", "worker"} {
		bin := buildApp(t, dir, app)
		for _, u := range []string{
			"postgres://u:" + synthSecret + "@127.0.0.1:1/x?sslmode=disable",
			"postgres://u:" + synthSecret + "%ZZ@127.0.0.1:1/x",
		} {
			args := []string(nil)
			if app == "migrate" {
				args = []string{"up"}
			}
			c, err := startChild("", bin, childEnv("APP_POSTGRES_URL="+u, "APP_LOG_LEVEL=debug", "APP_LOG_FORMAT=json"), nil, args...)
			if err != nil {
				t.Fatal(err)
			}
			res := waitChild(c, 30*time.Second)
			if res.outcome == timedOut {
				t.Fatalf("%s did not end", app)
			}
			requireNoSecret(t, app+" output", c.out.String(), synthSecret)
		}
	}
}

// A server error echoes the parameter that caused it: SELECT ($1::text)::integer answers SQLSTATE 22P02 with the value in the
// message. The value here is a database URL with a password, as a step that passes one on would. Env.QueryText answers with a
// fixed category and the SQLSTATE, never the driver's or the server's text. (Here, not in pkgs/platform/acceptance: that
// package needs no container, and its tests stay free of one.)
func TestSecret_QueryTextServerErrorHasNoParameterAndHasTheSQLState(t *testing.T) {
	e := &acceptance.Env{DatabaseURL: pgtest.URL(t)}
	_, err := e.QueryText(context.Background(), "SELECT ($1::text)::integer", "postgres://app:"+synthSecret+"@db.example/x")
	if err == nil {
		t.Fatal("QueryText accepted a URL as an integer")
	}
	if strings.Contains(err.Error(), synthSecret) || strings.Contains(err.Error(), "db.example") {
		t.Fatalf("error %q contains the parameter", err)
	}
	if !strings.Contains(err.Error(), "22P02") {
		t.Fatalf("error %q does not carry the SQLSTATE 22P02", err)
	}
}
