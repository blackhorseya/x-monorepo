package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// A client's Close returns before the server's backend has exited: a connection closed by the last closer of a scenario
// may still be in pg_stat_activity when drop looks. drop waits, within a bound, for it to go; it fails (and says who is left)
// only when the bound passes.
func TestTemplateDrop_WaitsForAClosedConnectionsBackendToExit(t *testing.T) {
	tpl := newTemplate(t)
	tpl.dropWait = 5 * time.Second
	tpl.prepare(1)
	dbURL, err := tpl.take()
	if err != nil {
		t.Fatal(err)
	}
	conn := pgtest.Conn(t, dbURL)
	closed := make(chan struct{})
	time.AfterFunc(400*time.Millisecond, func() { _ = conn.Close(context.Background()); close(closed) })

	if err := tpl.drop(context.WithoutCancel(context.Background()), dbURL); err != nil {
		t.Fatalf("drop: %v", err)
	}
	<-closed
}

// A connection that stays is still reported, with who it is and no query text.
func TestTemplateDrop_ReportsAConnectionThatStays(t *testing.T) {
	tpl := newTemplate(t)
	tpl.dropWait = 300 * time.Millisecond
	tpl.prepare(1)
	dbURL, err := tpl.take()
	if err != nil {
		t.Fatal(err)
	}
	conn := pgtest.Conn(t, dbURL+"&application_name=leaky-step")
	if _, err := conn.Exec(context.Background(), "SELECT 'query-text-marker'"); err != nil {
		t.Fatal(err)
	}

	err = tpl.drop(context.WithoutCancel(context.Background()), dbURL)
	if err == nil {
		t.Fatal("drop succeeded with a live connection")
	}
	for _, want := range []string{"pid", "application_name=leaky-step", "state=idle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "query-text-marker") {
		t.Errorf("error %q holds a query text", err)
	}
}
