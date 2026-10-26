package server_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// Every pass through the interceptor lands on auth.decisions{result, method}:
// the counter is what tells a guessed key (invalid) from a dropped header
// (missing), which the RPC-level status code cannot.
func TestAuthInterceptor_Decisions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	cfg := config.Auth{
		Mode:    config.AuthRequired,
		APIKeys: "console=console-key-0123456789",
		JWT:     config.JWT{HS256Secret: secret},
		Public:  []string{"/a.v1.S/Public"},
	}
	in, err := server.NewAuthInterceptor(cfg, mp, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := in(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	})
	call := func(procedure string, headers map[string]string) {
		req := connect.NewRequest(&struct{}{})
		for k, v := range headers {
			req.Header().Set(k, v)
		}
		_, _ = next(context.Background(), request{AnyRequest: req, procedure: procedure})
	}
	valid := token(t, []byte(secret), jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u-1", "exp": time.Now().Add(time.Minute).Unix()})

	call("/a.v1.S/Public", nil)
	call("/a.v1.S/Get", nil)
	call("/a.v1.S/Get", nil)
	call("/a.v1.S/Get", map[string]string{server.APIKeyHeader: "console-key-0123456789"})
	call("/a.v1.S/Get", map[string]string{server.APIKeyHeader: "nope-nope-nope-nope"})
	call("/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer " + valid})
	call("/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Bearer not.a.jwt"})
	call("/a.v1.S/Get", map[string]string{server.AuthorizationHeader: "Basic abc"})

	want := map[[2]string]int64{
		{server.ResultPublic, server.MethodNone}:    1,
		{server.ResultMissing, server.MethodNone}:   2,
		{server.ResultOK, server.MethodAPIKey}:      1,
		{server.ResultInvalid, server.MethodAPIKey}: 1,
		{server.ResultOK, server.MethodJWT}:         1,
		{server.ResultInvalid, server.MethodJWT}:    2, // a garbage token and a non-bearer scheme both count as jwt
	}
	got := decisions(t, reader)
	if len(got) != len(want) {
		t.Fatalf("decisions = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("decisions[%v] = %d, want %d", k, got[k], n)
		}
	}

	// Optional mode: no credential is a decision too, a different one.
	cfg.Mode = config.AuthOptional
	reader = sdkmetric.NewManualReader()
	in, err = server.NewAuthInterceptor(cfg, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), nil)
	if err != nil {
		t.Fatal(err)
	}
	next = in(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	})
	call("/a.v1.S/Get", nil)
	if got := decisions(t, reader); got[[2]string{server.ResultAnonymous, server.MethodNone}] != 1 || len(got) != 1 {
		t.Fatalf("optional decisions = %v, want one anonymous/none", got)
	}
}

// decisions reads auth.decisions as {result, method} → count.
func decisions(t *testing.T, reader *sdkmetric.ManualReader) map[[2]string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[[2]string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "auth.decisions" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("auth.decisions is %T, want Sum[int64]", m.Data)
			}
			if !sum.IsMonotonic {
				t.Fatal("auth.decisions must be monotonic")
			}
			for _, dp := range sum.DataPoints {
				result, _ := dp.Attributes.Value(attribute.Key("result"))
				method, _ := dp.Attributes.Value(attribute.Key("method"))
				out[[2]string{result.AsString(), method.AsString()}] = dp.Value
			}
		}
	}

	return out
}
