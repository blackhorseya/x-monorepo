package otel

import (
	"slices"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// With no exporter endpoint the tracer is a no-op, and a no-op tracer hands a parent's trace id on unchanged:
// an external traceparent would travel through the process to whatever it calls. So the propagator is empty
// then (reads nothing, writes nothing); with an endpoint it is W3C trace context + baggage.
func TestPropagator_FollowsTheEndpoint(t *testing.T) {
	off, cleanup, err := New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if f := off.Propagator.Fields(); len(f) != 0 {
		t.Fatalf("propagator fields without an endpoint = %v, want none", f)
	}

	on, cleanup, err := New(config.OTel{Endpoint: "127.0.0.1:1", Insecure: true}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	f := on.Propagator.Fields()
	if !slices.Contains(f, "traceparent") || !slices.Contains(f, "baggage") {
		t.Fatalf("propagator fields with an endpoint = %v, want traceparent and baggage", f)
	}
}
