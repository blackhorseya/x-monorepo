package acceptance

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.Terminate()
	os.Exit(code)
}

func TestEnvClose_LastRegisteredFirstAndJoinsErrors(t *testing.T) {
	var order []string
	boom := errors.New("boom")
	e := &Env{}
	e.Defer(func() error { order = append(order, "first"); return nil })
	e.Defer(func() error { order = append(order, "second"); return boom })

	if err := e.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want boom", err)
	}
	if want := []string{"second", "first"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if err := e.Close(); err != nil || len(order) != 2 {
		t.Fatalf("second Close ran again: %v, %v", err, order)
	}
}

// QueryText names no host, user or password when it cannot connect: the URL carries the password.
func TestQueryText_ConnectErrorHasNoURL(t *testing.T) {
	const secret = "S3cr3tSynth"
	e := &Env{DatabaseURL: "postgres://app:" + secret + "@127.0.0.1:1/x?sslmode=disable"}
	_, err := e.QueryText(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("QueryText connected to port 1")
	}
	for _, leak := range []string{secret, "127.0.0.1", "app"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q contains %q", err, leak)
		}
	}
}

// A server error echoes the parameter that caused it: SELECT ($1::text)::integer answers SQLSTATE 22P02 with the value in the
// message. The value here is a database URL with a password, as a step that passes one on would. QueryText answers with a
// fixed category and the SQLSTATE, never the driver's or the server's text.
func TestQueryText_ServerErrorHasNoParameterAndHasTheSQLState(t *testing.T) {
	const secret = "S3cr3tSynth"
	e := &Env{DatabaseURL: pgtest.URL(t)}
	_, err := e.QueryText(context.Background(), "SELECT ($1::text)::integer", "postgres://app:"+secret+"@db.example/x")
	if err == nil {
		t.Fatal("QueryText accepted a URL as an integer")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "db.example") {
		t.Fatalf("error %q contains the parameter", err)
	}
	if !strings.Contains(err.Error(), "22P02") {
		t.Fatalf("error %q does not carry the SQLSTATE 22P02", err)
	}
}
