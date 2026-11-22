package acceptance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

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
