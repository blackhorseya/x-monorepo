package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1/adminv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// noDatabase is what an authorized admin.v1 call answers when no database is wired (these tests have none): the
// request got through the whole chain to the handler, which has nothing to read. It is how a test tells a call
// that ran from one an interceptor refused.
const noDatabase = "admin.no_database"

// End-to-end through the wired graph: proto client → interceptor chain → admin service.
func TestPlatform_E2E(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Probes: alive, and ready (in-memory repositories: no dependency to check).
	for _, p := range []string{server.HealthzPath, server.ReadyzPath} {
		res, err := http.Get(ts.URL + p)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %v, %v", p, res, err)
		}
		_ = res.Body.Close()
	}
	// /api/version: the stamped identity (a test binary is "dev") and the Go runtime.
	vres, err := http.Get(ts.URL + server.VersionPath)
	if err != nil || vres.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %v, %v", server.VersionPath, vres, err)
	}
	var info buildinfo.Info
	if err := json.NewDecoder(vres.Body).Decode(&info); err != nil || info.Version != "dev" || info.Go != runtime.Version() {
		t.Fatalf("version = %+v, %v", info, err)
	}
	_ = vres.Body.Close()

	client := adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL), servertest.WithErrorMeta()))
	c := context.Background()

	// The call runs the whole chain (auth is off here) and the handler answers: no database is wired, a
	// FailedPrecondition with the service's own x-error-code.
	var ce *connect.Error
	_, err = client.ListPoison(c, &adminv1.ListPoisonRequest{})
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("ListPoison: err = %v, want FailedPrecondition", err)
	}
	if got := servertest.ErrorMeta(err).Get("x-error-code"); got != noDatabase {
		t.Fatalf("x-error-code = %q, want %s", got, noDatabase)
	}

	// Shape errors are rejected by the protovalidate interceptor, before the
	// service: same code, the shared validation x-error-code, and the violated
	// fields in a buf.validate.Violations detail.
	_, err = client.ListPoison(c, &adminv1.ListPoisonRequest{Limit: 501})
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || servertest.ErrorMeta(err).Get("x-error-code") != server.ValidationErrorCode {
		t.Fatalf("limit 501: err = %v", err)
	}
	if len(ce.Details()) != 1 {
		t.Fatalf("details = %d, want violations", len(ce.Details()))
	}
	detail, err := connectproto.UnmarshalErrorDetail(ce.Details()[0])
	if err != nil {
		t.Fatal(err)
	}
	violations, ok := detail.(*validate.Violations)
	if !ok || len(violations.GetViolations()) != 1 {
		t.Fatalf("violations = %v", detail)
	}

	// /api/metrics is exposed and carries the server's rpc metrics, per procedure and outcome, for the calls above.
	res, err := http.Get(ts.URL + server.MetricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var sb strings.Builder
	if _, err := io.Copy(&sb, res.Body); err != nil {
		t.Fatal(err)
	}
	text := sb.String()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", server.MetricsPath, res.StatusCode)
	}
	for _, want := range []string{
		`rpc_server_call_duration_seconds_count{error_type="FAILED_PRECONDITION",rpc_method="admin.v1.AdminService/ListPoison",rpc_response_status_code="FAILED_PRECONDITION",rpc_system_name="connectrpc"} 1`,
		`rpc_server_call_duration_seconds_count{error_type="INVALID_ARGUMENT",rpc_method="admin.v1.AdminService/ListPoison",rpc_response_status_code="INVALID_ARGUMENT",rpc_system_name="connectrpc"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("%s missing %q", server.MetricsPath, want)
		}
	}
}

// gRPC needs HTTP/2 and gRPC-Web is a distinct wire format; both must survive the
// router (gin wildcard routes + h2c) exactly like the Connect protocol does.
func TestPlatform_Protocols(t *testing.T) {
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ts := httptest.NewUnstartedServer(srv.Handler())
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	ts.Config.Protocols = protocols
	ts.Start()
	defer ts.Close()

	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	h2cClient := &http.Client{Transport: &http.Transport{Protocols: h2c}}

	// Every protocol works at the bare protocol path and under server.RPCPrefix
	// (a connect-go client prefixes gRPC too; grpc-go could not, hence both).
	for _, tt := range []struct {
		name   string
		client *http.Client
		opts   []connecthttp.Option
	}{
		{"connect", http.DefaultClient, nil},
		{"grpc", h2cClient, []connecthttp.Option{connecthttp.WithGRPC()}},
		{"grpc-web", http.DefaultClient, []connecthttp.Option{connecthttp.WithGRPCWeb()}},
		{"connect/api", http.DefaultClient, nil},
		{"grpc/api", h2cClient, []connecthttp.Option{connecthttp.WithGRPC()}},
		{"grpc-web/api", http.DefaultClient, []connecthttp.Option{connecthttp.WithGRPCWeb()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := ts.URL
			if strings.HasSuffix(tt.name, "/api") {
				base += server.RPCPrefix
			}
			client := adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(tt.client, base, tt.opts...), servertest.WithErrorMeta()))
			c := context.Background()

			// Response meta travels as headers on every protocol, on errors too: the request's own id is echoed.
			id := "client-" + strings.ReplaceAll(tt.name, "/", "-")
			c, info := connect.NewClientContext(c)
			info.RequestHeader().Set(server.RequestIDHeader, id)
			_, err := client.ListPoison(c, &adminv1.ListPoisonRequest{})
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
				t.Fatalf("err = %v, want FailedPrecondition", err)
			}
			if got := servertest.ErrorMeta(err).Get(server.RequestIDHeader); got != id {
				t.Fatalf("%s = %q, want echo of the client's id (%v)", server.RequestIDHeader, got, servertest.ErrorMeta(err))
			}
		})
	}
}

// Docs are mounted under /api only when enabled; disabled answers with a hint.
func TestDocs(t *testing.T) {
	get := func(t *testing.T, h http.Handler, path string) (int, string) {
		t.Helper()
		ts := httptest.NewServer(h)
		defer ts.Close()
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var sb strings.Builder
		_, _ = io.Copy(&sb, res.Body)
		return res.StatusCode, sb.String()
	}
	newServer := func(t *testing.T, docs bool) http.Handler {
		t.Helper()
		srv, cleanup, err := Initialize(&config.AppConfig{
			HTTP: config.HTTP{Addr: ":0", Docs: docs},
			Log:  config.Log{Level: "error", Format: "text"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		return srv.Handler()
	}

	on := newServer(t, true)
	for _, path := range []string{
		server.DocsPath,
		server.DocsPath + "/admin", server.OpenAPIPath + "/admin.yaml",
	} {
		if code, _ := get(t, on, path); code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
	}
	if code, body := get(t, on, server.OpenAPIPath+"/admin.yaml"); code != 200 || !strings.Contains(body, "openapi: 3") {
		t.Fatalf("openapi body: %d %q", code, body[:min(len(body), 40)])
	}
	if code, _ := get(t, on, "/docs"); code != http.StatusNotFound {
		t.Fatalf("unprefixed /docs = %d, want 404", code)
	}

	off := newServer(t, false)
	if code, body := get(t, off, server.DocsPath); code != http.StatusNotFound || !strings.Contains(body, "APP_HTTP_DOCS=true") {
		t.Fatalf("disabled docs = %d %q", code, body)
	}
	if code, _ := get(t, off, server.OpenAPIPath+"/admin.yaml"); code != http.StatusNotFound {
		t.Fatalf("disabled openapi = %d, want 404", code)
	}
}

func TestParseArgs(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		bad  bool
	}{
		"no args":         {nil, false},
		"--version":       {[]string{"--version"}, false},
		"-v":              {[]string{"-v"}, false},
		"an unknown flag": {[]string{"--wait-catalog"}, true},
		"a stray arg":     {[]string{"--version", "extra"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if err := parseArgs(tc.args); (err != nil) != tc.bad {
				t.Fatalf("parseArgs(%v) = %v, want error: %v", tc.args, err, tc.bad)
			}
		})
	}
}
