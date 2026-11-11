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

	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// End-to-end through the wired graph: proto client → rpc adapter → decorated
// usecase → domain → infra.
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

	client := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL, connecthttp.WithReadMaxBytes(0)), servertest.WithErrorMeta()))
	c := context.Background()

	created, err := client.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	a := created.GetAccount()
	got, err := client.GetAccount(c, &identityv1.GetAccountRequest{Id: a.GetId()})
	if err != nil || got.GetAccount().GetName() != "alice" {
		t.Fatalf("GetAccount: %v, %v", got, err)
	}

	var ce *connect.Error
	_, err = client.GetAccount(c, &identityv1.GetAccountRequest{Id: "nope"})
	if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
		t.Fatalf("not found: err = %v", err)
	}
	if servertest.ErrorMeta(err).Get("x-error-code") == "" {
		t.Fatal("missing x-error-code header")
	}

	// Shape errors are rejected by the protovalidate interceptor, before the
	// use case: same code, the shared validation x-error-code, and the violated
	// fields in a buf.validate.Violations detail.
	_, err = client.GetAccount(c, &identityv1.GetAccountRequest{})
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || servertest.ErrorMeta(err).Get("x-error-code") != server.ValidationErrorCode {
		t.Fatalf("empty request: err = %v", err)
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

	// ListAccounts: cursor pagination end to end (3 accounts, pages of 2).
	for _, name := range []string{"bob", "carol"} {
		if _, err := client.CreateAccount(c, &identityv1.CreateAccountRequest{Name: name}); err != nil {
			t.Fatalf("CreateAccount %s: %v", name, err)
		}
	}
	page1, err := client.ListAccounts(c, &identityv1.ListAccountsRequest{PageSize: 2})
	if err != nil || len(page1.GetAccounts()) != 2 || page1.GetNextPageToken() == "" {
		t.Fatalf("ListAccounts page 1: %v, %v", page1, err)
	}
	page2, err := client.ListAccounts(c, &identityv1.ListAccountsRequest{PageSize: 2, PageToken: page1.GetNextPageToken()})
	if err != nil || len(page2.GetAccounts()) != 1 || page2.GetNextPageToken() != "" {
		t.Fatalf("ListAccounts page 2: %v, %v", page2, err)
	}

	// Metrics decorator is wired for every use case and exposed on /api/metrics.
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
	for _, want := range []string{
		`usecase_duration_seconds_count{kind="ok",usecase="identity.create_account"} 3`,
		`usecase_duration_seconds_count{kind="ok",usecase="identity.get_account"} 1`,
		`usecase_duration_seconds_count{kind="not_found",usecase="identity.get_account"} 1`,
		`usecase_duration_seconds_count{kind="ok",usecase="identity.list_accounts"} 2`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("%s missing %q\n%s", server.MetricsPath, want, text)
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
			client := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(tt.client, base, append([]connecthttp.Option{connecthttp.WithReadMaxBytes(0)}, tt.opts...)...), servertest.WithErrorMeta()))
			c := context.Background()

			// Response meta travels as headers on every protocol, on errors too.
			_, err := client.GetAccount(c, &identityv1.GetAccountRequest{Id: "nope"})
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != connect.CodeNotFound {
				t.Fatalf("err = %v, want NotFound", err)
			}
			if servertest.ErrorMeta(err).Get(server.RequestIDHeader) == "" {
				t.Fatalf("error without %s: %v", server.RequestIDHeader, servertest.ErrorMeta(err))
			}

			req := &identityv1.WhoAmIRequest{}
			id := "client-" + strings.ReplaceAll(tt.name, "/", "-")
			c, info := connect.NewClientContext(c)
			info.RequestHeader().Set(server.RequestIDHeader, id)
			if _, err := client.WhoAmI(c, req); err != nil {
				t.Fatal(err)
			}
			if got := info.ResponseHeader().Get(server.RequestIDHeader); got != id {
				t.Fatalf("%s = %q, want echo of the client's id", server.RequestIDHeader, got)
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
		server.DocsPath + "/identity", server.OpenAPIPath + "/identity.yaml",
	} {
		if code, _ := get(t, on, path); code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
	}
	if code, body := get(t, on, server.OpenAPIPath+"/identity.yaml"); code != 200 || !strings.Contains(body, "openapi: 3") {
		t.Fatalf("openapi body: %d %q", code, body[:min(len(body), 40)])
	}
	if code, _ := get(t, on, "/docs"); code != http.StatusNotFound {
		t.Fatalf("unprefixed /docs = %d, want 404", code)
	}

	off := newServer(t, false)
	if code, body := get(t, off, server.DocsPath); code != http.StatusNotFound || !strings.Contains(body, "APP_HTTP_DOCS=true") {
		t.Fatalf("disabled docs = %d %q", code, body)
	}
	if code, _ := get(t, off, server.OpenAPIPath+"/identity.yaml"); code != http.StatusNotFound {
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
