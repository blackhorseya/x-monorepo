package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// The template mounts admin.v1 only and wires no external identities, so the cells it cannot reach through
// Initialize run here: apps/rpc's own providers (NewRoutes, NewPoisonService, NewServerOptions) assembling a
// server.New with the mount of a fixture BC.

type fixtureMethod struct{ spec connect.Spec }

func (f fixtureMethod) Register(s *connect.Server) {
	s.Register(connect.Method{Spec: f.spec, Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
		if err := stream.Receive(&emptypb.Empty{}); err != nil {
			return err
		}

		return stream.Send(&emptypb.Empty{})
	}})
}

func fixtureMount(procedure string, decl *commonv1.MethodAuthz, permissions ...string) server.Mount {
	return server.Mount{Routes: []server.Route{fixtureMethod{servertest.Spec(procedure, decl)}}, Permissions: permissions}
}

func assemble(t *testing.T, auth config.Auth, oidcAuth server.OIDCAuth, mounts ...server.Mount) (*server.Server, error) {
	t.Helper()
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	return server.New(config.HTTP{Addr: ":0"}, auth, slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewRoutes(mounts, NewPoisonService(nil, mounts)), nil, nil, tel, nil, oidcAuth, NewServerOptions(mounts)...)
}

func TestAuthzAssembly_StartupRefusals(t *testing.T) {
	rule := func(r *commonv1.AuthzRule) *commonv1.MethodAuthz { return servertest.Rules(r) }
	admin := rule(&commonv1.AuthzRule{Permission: "orders.admin"})
	for _, tt := range []struct {
		name   string
		mounts []server.Mount
		want   string // "" = starts
	}{
		{"declared and registered", []server.Mount{fixtureMount("/orders.v1.S/Do", admin, "orders.admin")}, ""},
		{"no declaration", []server.Mount{fixtureMount("/orders.v1.S/Do", nil)}, "/orders.v1.S/Do"},
		{"unregistered kind", []server.Mount{fixtureMount("/orders.v1.S/Do", rule(&commonv1.AuthzRule{Kind: "staff"}))}, `"staff" is not registered`},
		{"permission no mount registers", []server.Mount{fixtureMount("/orders.v1.S/Do", admin)}, `"orders.admin" is not registered`},
		{"two mounts register one permission", []server.Mount{
			fixtureMount("/orders.v1.S/Do", admin, "orders.admin"), fixtureMount("/carts.v1.S/Do", admin, "orders.admin"),
		}, `"orders.admin" is registered twice`},
		{"a mount registers platform.admin", []server.Mount{fixtureMount("/orders.v1.S/Do", admin, "orders.admin", server.PlatformAdminPermission)}, "registered twice"},
		{"malformed permission", []server.Mount{fixtureMount("/orders.v1.S/Do", admin, "orders.admin", "orders")}, `permission "orders"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := assemble(t, config.Auth{}, server.OIDCAuth{}, tt.mounts...)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("server.New = %v, want it to start", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("server.New = %v, want a refusal containing %q", err, tt.want)
			}
		})
	}
}

type unboundIdentities struct{}

func (unboundIdentities) Verify(_ context.Context, token string) (oidc.Identity, error) {
	return oidc.Identity{Issuer: "https://idp.example", Subject: token}, nil
}

func (unboundIdentities) ResolveExternal(_ context.Context, id contextx.ExternalIdentity) (contextx.Principal, error) {
	if id.Subject == "bound" {
		return contextx.Principal{Subject: "acc-1"}, nil
	}

	return contextx.Principal{}, server.ErrUnboundIdentity
}

func (unboundIdentities) AllowsUnbound(string) bool { return true }

// The template's classifier: a verified principal is a "user"; an identity bound to no account has no kind, so a
// method that lets it past authentication still refuses it, with permission_denied.
func TestAuthzAssembly_DefaultKind(t *testing.T) {
	const procedure = "/orders.v1.S/Get"
	srv, err := assemble(t, config.Auth{Mode: config.AuthRequired}, server.OIDCAuth{Verifier: unboundIdentities{}, Resolver: unboundIdentities{}},
		fixtureMount(procedure, servertest.Rules(&commonv1.AuthzRule{Kind: KindUser})))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for token, want := range map[string]string{"bound": "", "unbound": server.PermissionDeniedCode} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+procedure, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(server.AuthorizationHeader, "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		wantStatus := http.StatusOK
		if want != "" {
			wantStatus = http.StatusForbidden
		}
		if got := res.Header.Get("X-Error-Code"); res.StatusCode != wantStatus || got != want {
			t.Errorf("%s identity: status %d x-error-code %q, want %d %q", token, res.StatusCode, got, wantStatus, want)
		}
	}
}
