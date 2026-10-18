package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestUntracedByGin(t *testing.T) {
	filter := untracedByGin([]string{"/ordering.v1.OrderService/"})
	cases := map[string]bool{
		"/ordering.v1.OrderService/CreateOrder":     false, // otelconnect's job
		"/api/ordering.v1.OrderService/CreateOrder": false, // same handler under RPCPrefix
		"/api/healthz":               false,
		"/api/readyz":                false,
		"/api/metrics":               false,
		"/healthz":                   true, // not a route any more: 404, but traced like any unknown path
		"/api/docs":                  true,
		"/api/openapi/ordering.yaml": true,
		"/nope":                      true,
	}
	for path, want := range cases {
		c := &gin.Context{Request: httptest.NewRequest("GET", path, nil)}
		if got := filter(c); got != want {
			t.Errorf("%s: traced = %v, want %v", path, got, want)
		}
	}
}

func TestHealth_Probes(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	boom := errors.New("connection refused")
	var dbUp atomic.Bool
	dbUp.Store(true)
	hp := &health{checks: Readiness{
		{Name: "postgres", Probe: func(context.Context) error {
			if dbUp.Load() {
				return nil
			}
			return boom
		}},
	}}
	engine := gin.New()
	engine.GET(HealthzPath, hp.live)
	engine.GET(ReadyzPath, hp.ready)

	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, _ := get(HealthzPath); code != 200 {
		t.Fatalf("live = %d", code)
	}
	if code, body := get(ReadyzPath); code != 200 || !strings.Contains(body, `"postgres":"ok"`) {
		t.Fatalf("ready = %d %s", code, body)
	}

	// A failing dependency: not ready, liveness unaffected (no restart for a DB outage).
	dbUp.Store(false)
	if code, body := get(ReadyzPath); code != 503 || !strings.Contains(body, boom.Error()) {
		t.Fatalf("ready with db down = %d %s", code, body)
	}
	if code, _ := get(HealthzPath); code != 200 {
		t.Fatalf("live with db down = %d, want 200", code)
	}

	// Draining: not ready even with every dependency fine.
	dbUp.Store(true)
	hp.draining.Store(true)
	if code, body := get(ReadyzPath); code != 503 || !strings.Contains(body, `"server":"draining"`) {
		t.Fatalf("ready while draining = %d %s", code, body)
	}
}

func TestHealth_ProbeTimeout(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	hp := &health{checks: Readiness{{Name: "slow", Probe: func(c context.Context) error {
		<-c.Done() // a hung dependency: the probe's own timeout must end it
		return c.Err()
	}}}}
	engine := gin.New()
	engine.GET(ReadyzPath, hp.ready)
	rec := httptest.NewRecorder()
	start := time.Now()
	engine.ServeHTTP(rec, httptest.NewRequest("GET", ReadyzPath, nil))
	if rec.Code != 503 || time.Since(start) > checkTimeout+time.Second {
		t.Fatalf("code=%d took %s", rec.Code, time.Since(start))
	}
}
