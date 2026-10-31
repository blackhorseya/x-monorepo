package client_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/client"
	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

const serviceKey = "catalog-service-key-0123456789ab"

// upstream is catalog-rpc as the client sees it: a Connect server that records what arrived and
// answers per test.
type upstream struct {
	catalogv1connect.UnimplementedCatalogServiceHandler
	mu      sync.Mutex
	headers http.Header
	getErr  error
	delay   time.Duration
}

func (u *upstream) GetProduct(c context.Context, req *connect.Request[catalogv1.GetProductRequest]) (*connect.Response[catalogv1.GetProductResponse], error) {
	u.mu.Lock()
	u.headers = req.Header().Clone()
	err, delay := u.getErr, u.delay
	u.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-c.Done():
			return nil, c.Err()
		}
	}
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&catalogv1.GetProductResponse{Product: &catalogv1.Product{
		Sku: req.Msg.GetSku(), Name: "n", Active: true, Price: &commonv1.Money{AmountMinor: 100, Currency: "USD"},
	}}), nil
}

func tel(t *testing.T) *otel.Providers {
	t.Helper()
	return &otel.Providers{
		Tracer:     sdktrace.NewTracerProvider(),
		Meter:      noop.NewMeterProvider(),
		Propagator: propagation.TraceContext{},
		Logger:     slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
}

func newClient(t *testing.T, url string, timeout time.Duration) *client.Client {
	t.Helper()
	cl, err := client.New(config.Catalog{Upstream: url, APIKey: serviceKey, Timeout: timeout}, tel(t))
	if err != nil {
		t.Fatal(err)
	}

	return cl
}

func serve(t *testing.T, u *upstream) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(catalogv1connect.NewCatalogServiceHandler(u))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return ts.URL
}

func get(cl *client.Client, c context.Context) error {
	_, err := cl.GetProduct(c, connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"}))

	return err
}

func wantUnavailable(t *testing.T, err error) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Meta().Get("x-error-code") != "catalog.unavailable" {
		t.Fatalf("err = %v, want UNAVAILABLE with x-error-code catalog.unavailable", err)
	}
	// the cause stays in the log: the caller sees a fixed message
	if strings.Contains(ce.Message(), "connection refused") || strings.Contains(ce.Message(), serviceKey) {
		t.Fatalf("the message leaks the cause or the key: %q", ce.Message())
	}
}

// The service credential is the only credential that reaches the catalog: what the caller
// presented (a user's key, a bearer token) is never forwarded.
func TestClient_SendsOnlyTheServiceKey(t *testing.T) {
	u := &upstream{}
	cl := newClient(t, serve(t, u), time.Second)

	req := connect.NewRequest(&catalogv1.GetProductRequest{Sku: "A"})
	req.Header().Set("X-API-Key", "the-users-own-key-0123456789")
	req.Header().Set("Authorization", "Bearer the-users-own-token")
	req.Header().Set("Cookie", "session=abc")
	if _, err := cl.GetProduct(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := u.headers.Get("X-API-Key"); got != serviceKey {
		t.Fatalf("X-API-Key at the catalog = %q, want the service key", got)
	}
	for _, h := range []string{"Authorization", "Cookie"} {
		if v := u.headers.Get(h); v != "" {
			t.Fatalf("%s reached the catalog: %q", h, v)
		}
	}
}

// otel propagation: the trace of the request that asked is the trace the catalog continues.
func TestClient_PropagatesTheTrace(t *testing.T) {
	u := &upstream{}
	cl := newClient(t, serve(t, u), time.Second)

	tp := sdktrace.NewTracerProvider()
	c, span := tp.Tracer("test").Start(context.Background(), "caller")
	defer span.End()
	if err := get(cl, c); err != nil {
		t.Fatal(err)
	}
	tpHeader := u.headers.Get("Traceparent")
	if !strings.Contains(tpHeader, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent at the catalog = %q, want trace id %s", tpHeader, span.SpanContext().TraceID())
	}
}

// The classification matrix: what each way of failing becomes for the caller.
func TestClient_ErrorClassification(t *testing.T) {
	for name, code := range map[string]connect.Code{
		"NotFound": connect.CodeNotFound, "InvalidArgument": connect.CodeInvalidArgument,
		"FailedPrecondition": connect.CodeFailedPrecondition, "AlreadyExists": connect.CodeAlreadyExists,
	} {
		t.Run("passes through "+name, func(t *testing.T) {
			ce := connect.NewError(code, errors.New("the catalog said so"))
			ce.Meta().Set("x-error-code", "product.some_code")
			cl := newClient(t, serve(t, &upstream{getErr: ce}), time.Second)

			err := get(cl, context.Background())
			var got *connect.Error
			if !errors.As(err, &got) || got.Code() != code || got.Meta().Get("x-error-code") != "product.some_code" {
				t.Fatalf("err = %v, want %s with x-error-code product.some_code unchanged", err, code)
			}
		})
	}

	for name, code := range map[string]connect.Code{
		"Internal": connect.CodeInternal, "Unknown": connect.CodeUnknown, "Unavailable": connect.CodeUnavailable,
		"ResourceExhausted": connect.CodeResourceExhausted, "DeadlineExceeded": connect.CodeDeadlineExceeded,
	} {
		t.Run(name+" is unavailable", func(t *testing.T) {
			cl := newClient(t, serve(t, &upstream{getErr: connect.NewError(code, errors.New("boom"))}), time.Second)
			wantUnavailable(t, get(cl, context.Background()))
		})
	}

	t.Run("a catalog that is not listening", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		url := "http://" + ln.Addr().String()
		_ = ln.Close()
		wantUnavailable(t, get(newClient(t, url, time.Second), context.Background()))
	})

	t.Run("a catalog slower than the budget", func(t *testing.T) {
		cl := newClient(t, serve(t, &upstream{delay: 2 * time.Second}), 100*time.Millisecond)
		start := time.Now()
		wantUnavailable(t, get(cl, context.Background()))
		if d := time.Since(start); d > time.Second {
			t.Fatalf("took %s, the budget is 100ms", d)
		}
	})

	t.Run("a 5xx that is not Connect at all", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
		}))
		t.Cleanup(ts.Close)
		wantUnavailable(t, get(newClient(t, ts.URL, time.Second), context.Background()))
	})

	t.Run("the caller hanging up is canceled, not a sick catalog", func(t *testing.T) {
		cl := newClient(t, serve(t, &upstream{delay: 2 * time.Second}), 5*time.Second)
		c, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		err := get(cl, c)
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatalf("err = %v (code %s), want Canceled", err, connect.CodeOf(err))
		}
	})
}

// A service key the catalog does not accept is the deployment's mistake, not the user's: the caller
// sees UNAVAILABLE like any outage (no hint about credentials), and the log says what is wrong.
func TestClient_RejectedServiceKeyIsUnavailableButReadableInTheLog(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeUnauthenticated, connect.CodePermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			rejected := connect.NewError(code, errors.New("auth.invalid_credential"))
			rejected.Meta().Set("x-error-code", "auth.invalid_credential")
			cl := newClient(t, serve(t, &upstream{getErr: rejected}), time.Second)

			var logs bytes.Buffer
			c := contextx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
			err := get(cl, c)
			wantUnavailable(t, err)
			if strings.Contains(err.Error(), "invalid_credential") || strings.Contains(err.Error(), "auth.") {
				t.Fatalf("the credential problem reaches the caller: %v", err)
			}
			out := logs.String()
			if !strings.Contains(out, "credential rejected") || !strings.Contains(out, code.String()) {
				t.Fatalf("log = %q, want a line saying the service credential was rejected, with the code", out)
			}
			if strings.Contains(out, serviceKey) {
				t.Fatalf("the log carries the service key: %q", out)
			}
		})
	}
}

func TestNew_RefusesWhatItCannotUse(t *testing.T) {
	for name, cfg := range map[string]config.Catalog{
		"no upstream":  {APIKey: serviceKey, Timeout: time.Second},
		"no key":       {Upstream: "http://catalog-rpc:80", Timeout: time.Second},
		"not a url":    {Upstream: "catalog-rpc", APIKey: serviceKey, Timeout: time.Second},
		"wrong scheme": {Upstream: "ftp://catalog-rpc", APIKey: serviceKey, Timeout: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.New(cfg, tel(t))
			if err == nil {
				t.Fatal("client.New accepted it")
			}
			if strings.Contains(err.Error(), serviceKey) {
				t.Fatalf("the error carries the key: %v", err)
			}
		})
	}
}

// What the catalog says about a failure is its text, not ours: a catalog that reflects the service key (or
// anything else) in an error message must not get it into this process's log. The failure log carries the
// operation, a fixed category and the Connect code, nothing the remote wrote, and not the upstream address.
func TestClient_FailureLogsCarryNoRemoteText(t *testing.T) {
	logOf := func(t *testing.T, url string, u *upstream) string {
		t.Helper()
		var logs bytes.Buffer
		c := contextx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
		wantUnavailable(t, get(newClient(t, url, time.Second), c))

		return logs.String()
	}
	const reflected = "boom: got key " + serviceKey + " and Authorization: Bearer user-token-0123456789"

	for name, code := range map[string]connect.Code{
		"Internal": connect.CodeInternal, "Unknown": connect.CodeUnknown, "Unavailable": connect.CodeUnavailable,
		"ResourceExhausted": connect.CodeResourceExhausted,
	} {
		t.Run(name+" with a reflected key", func(t *testing.T) {
			u := &upstream{getErr: connect.NewError(code, errors.New(reflected))}
			out := logOf(t, serve(t, u), u)
			for _, leak := range []string{serviceKey, "user-token", "boom", "Authorization"} {
				if strings.Contains(out, leak) {
					t.Fatalf("the log carries remote text %q: %s", leak, out)
				}
			}
			if !strings.Contains(out, "catalog call failed") || !strings.Contains(out, code.String()) {
				t.Fatalf("log = %q, want the fixed line with the code %s", out, code)
			}
		})
	}

	t.Run("a non-Connect 5xx whose body reflects the key", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "<html>"+reflected+"</html>", http.StatusBadGateway)
		}))
		t.Cleanup(ts.Close)
		out := logOf(t, ts.URL, nil)
		if strings.Contains(out, serviceKey) || strings.Contains(out, "html") || strings.Contains(out, "boom") {
			t.Fatalf("the log carries the body of the 502: %s", out)
		}
	})

	t.Run("a catalog nothing listens on: the address is not logged either", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		out := logOf(t, "http://"+addr, nil)
		if strings.Contains(out, addr) || strings.Contains(out, "dial tcp") || strings.Contains(out, "refused") {
			t.Fatalf("the log carries the transport error / address: %s", out)
		}
	})
}

// The upstream URL is configuration, not a place for a credential: userinfo in it would travel in every
// error text of the HTTP stack.
func TestNew_RefusesCredentialsInTheUpstreamURL(t *testing.T) {
	_, err := client.New(config.Catalog{Upstream: "http://user:hunter2@catalog-rpc:80", APIKey: serviceKey, Timeout: time.Second}, tel(t))
	if err == nil {
		t.Fatal("client.New accepted userinfo in catalog.upstream")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error carries the password: %v", err)
	}
}
