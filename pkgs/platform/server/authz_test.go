package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// Everything here goes through server.New and HTTP, with fixture procedures whose declarations are read from
// connect.Spec.Schema, as a generated service's are.

const (
	openProc    = "/fx.v1.Authz/Open"    // public
	adminProc   = "/fx.v1.Authz/Admin"   // platform.admin
	ordersProc  = "/fx.v1.Authz/Orders"  // orders.admin
	staffProc   = "/fx.v1.Authz/Staff"   // staff with orders.read, or any service
	userProc    = "/fx.v1.Authz/User"    // any user
	claimProc   = "/fx.v1.Authz/Claim"   // kind claimant
	claimRWProc = "/fx.v1.Authz/ClaimRW" // claimant with orders.admin
)

func rule(kind, permission string) *commonv1.AuthzRule {
	return &commonv1.AuthzRule{Kind: kind, Permission: permission}
}

func authzSpecs() []connect.Spec {
	return []connect.Spec{
		servertest.Spec(openProc, servertest.Public()),
		servertest.Spec(adminProc, servertest.Rules(rule("", server.PlatformAdminPermission))),
		servertest.Spec(ordersProc, servertest.Rules(rule("", "orders.admin"))),
		servertest.Spec(staffProc, servertest.Rules(rule("staff", "orders.read"), rule("service", ""))),
		servertest.Spec(userProc, servertest.Rules(rule("user", ""))),
		servertest.Spec(claimProc, servertest.Rules(rule("claimant", ""))),
		servertest.Spec(claimRWProc, servertest.Rules(rule("claimant", "orders.admin"))),
	}
}

// specRoute mounts specs; the handler answers 200 and reports the context it ran with.
type specRoute struct {
	specs []connect.Spec
	ran   func(context.Context)
}

func (r specRoute) Register(s *connect.Server) {
	for _, spec := range r.specs {
		s.Register(connect.Method{Spec: spec, Handler: func(c context.Context, _ connect.Spec, stream connect.ServerStream) error {
			if err := stream.Receive(&emptypb.Empty{}); err != nil {
				return err
			}
			if r.ran != nil {
				r.ran(c)
			}

			return stream.Send(&emptypb.Empty{})
		}})
	}
}

// roleKind classifies by a "kind:<name>" role, else "user"; an identity with no account is a "claimant" when
// claimants is set and nothing otherwise.
func roleKind(claimants bool) func(context.Context) string {
	return func(c context.Context) string {
		p := contextx.PrincipalFrom(c)
		if p.IsAnonymous() {
			if _, ok := contextx.ExternalIdentityFrom(c); ok && claimants {
				return "claimant"
			}

			return ""
		}
		for _, r := range p.Roles {
			if k, ok := strings.CutPrefix(r, "kind:"); ok {
				return k
			}
		}

		return "user"
	}
}

func authzOptions(classify func(context.Context) string) []server.Option {
	return []server.Option{
		server.WithPrincipalKinds(classify, server.PrincipalKind{Name: "user"}, server.PrincipalKind{Name: "staff", RequiresPermission: true},
			server.PrincipalKind{Name: "service"}, server.PrincipalKind{Name: "claimant"}),
		server.WithPermissions("orders.admin", "orders.read"),
	}
}

// keyFor is the API key of the principal holding exactly roles; apiKeys configures one per entry of principals.
func keyFor(name string) string { return name + "-key-0123456789abcdef" }

var principals = map[string]string{
	"nobody": "", "root": "root", "literaladmin": "admin", "literalperm": "platform.admin",
	"orders": "orders.admin", "parent": "orders", "child": "orders.admin.eu", "reader": "orders.read",
	"staffreader": "kind:staff|orders.read", "staff": "kind:staff", "service": "kind:service", "ghost": "kind:ghost|orders.admin",
}

func apiKeys() string {
	var keys []string
	for name, roles := range principals {
		keys = append(keys, name+"="+keyFor(name)+":"+roles)
	}

	return strings.Join(keys, ",")
}

type authzServer struct {
	url  string
	logs *logBuffer
	// calls and kind are of the last request: whether the handler ran, and the kind in its context.
	calls int
	kind  string
}

func startAuthz(t *testing.T, cfg config.HTTP, auth config.Auth, oidcAuth server.OIDCAuth, opts ...server.Option) *authzServer {
	t.Helper()
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	s := &authzServer{logs: &logBuffer{}}
	route := specRoute{specs: authzSpecs(), ran: func(c context.Context) {
		s.calls++
		s.kind = contextx.PrincipalKindFrom(c)
	}}
	srv, err := server.New(cfg, auth, slog.New(slog.NewTextHandler(s.logs, nil)), []server.Route{route}, nil, nil, tel, nil, oidcAuth, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	s.url = ts.URL

	return s
}

// post calls procedure and answers the Connect code ("" = ok) and x-error-code.
func (s *authzServer) post(t *testing.T, procedure string, headers map[string]string) (string, string) {
	t.Helper()
	s.calls, s.kind = 0, ""
	req, err := http.NewRequest(http.MethodPost, s.url+procedure, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &body)
	if (res.StatusCode == http.StatusOK) != (s.calls == 1) {
		t.Fatalf("%s: status %d but the handler ran %d time(s)", procedure, res.StatusCode, s.calls)
	}

	return body.Code, res.Header.Get("X-Error-Code")
}

func as(name string) map[string]string {
	if name == "" {
		return nil
	}

	return map[string]string{server.APIKeyHeader: keyFor(name)}
}

type outcomeWant struct{ code, errorCode string }

var (
	admitted        = outcomeWant{}
	unauthenticated = outcomeWant{"unauthenticated", server.UnauthenticatedCode}
	permission      = outcomeWant{"permission_denied", server.PermissionDeniedCode}
)

func (s *authzServer) want(t *testing.T, procedure string, headers map[string]string, w outcomeWant) {
	t.Helper()
	if code, errorCode := s.post(t, procedure, headers); code != w.code || errorCode != w.errorCode {
		t.Fatalf("%s = %q / %q, want %q / %q", procedure, code, errorCode, w.code, w.errorCode)
	}
}

// metric reports whether /api/metrics counts an authentication decision with this result.
func (s *authzServer) metric(t *testing.T, result string) bool {
	t.Helper()
	res, err := http.Get(s.url + server.MetricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "auth_decisions_total{") && strings.Contains(line, `result="`+result+`"`) {
			return true
		}
	}

	return false
}

// The execution matrix: auth.mode x (public method, no credential, a credential that meets no rule, one that
// meets a rule). Each mode runs its four columns on one server.
func TestAuthz_Matrix(t *testing.T) {
	badKey := map[string]string{server.APIKeyHeader: "nope-nope-nope-nope-nope"}
	for _, tt := range []struct {
		mode                  string
		anonymous, mismatched outcomeWant
	}{
		{config.AuthOff, admitted, admitted},
		{config.AuthOptional, unauthenticated, permission},
		{config.AuthRequired, unauthenticated, permission},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			s := startAuthz(t, config.HTTP{Addr: ":0"}, config.Auth{Mode: tt.mode, APIKeys: apiKeys()}, server.OIDCAuth{}, authzOptions(roleKind(false))...)
			t.Run("public", func(t *testing.T) {
				s.want(t, openProc, nil, admitted)
				s.want(t, openProc, badKey, admitted) // the credential is not looked at
				if s.kind != "" {
					t.Errorf("a public method carries kind %q, want none", s.kind)
				}
			})
			t.Run("no credential", func(t *testing.T) {
				s.want(t, ordersProc, nil, tt.anonymous)
				// required: authentication refuses it, before authorization; optional: it runs anonymous into authorization.
				if got := s.metric(t, server.ResultMissing); got != (tt.mode == config.AuthRequired) {
					t.Errorf("auth.decisions result=missing counted = %v in mode %s", got, tt.mode)
				}
				if got := s.metric(t, server.ResultAnonymous); got != (tt.mode == config.AuthOptional) {
					t.Errorf("auth.decisions result=anonymous counted = %v in mode %s", got, tt.mode)
				}
			})
			t.Run("no matching rule", func(t *testing.T) { s.want(t, ordersProc, as("nobody"), tt.mismatched) })
			t.Run("matching rule", func(t *testing.T) {
				s.want(t, ordersProc, as("orders"), admitted)
				wantKind := "user"
				if tt.mode == config.AuthOff {
					wantKind = ""
				}
				if s.kind != wantKind {
					t.Errorf("kind in the context = %q, want %q", s.kind, wantKind)
				}
			})
		})
	}
}

func TestAuthz_Rules(t *testing.T) {
	s := startAuthz(t, config.HTTP{Addr: ":0"}, config.Auth{Mode: config.AuthRequired, AdminRole: "root", APIKeys: apiKeys()}, server.OIDCAuth{}, authzOptions(roleKind(false))...)
	for _, tt := range []struct {
		name, proc, who string
		want            outcomeWant
	}{
		{"platform.admin is the admin role", adminProc, "root", admitted},
		{"the default role name is not it when admin_role is set", adminProc, "literaladmin", permission},
		{"nor is the permission string", adminProc, "literalperm", permission},
		{"a permission is the role of the same name", ordersProc, "orders", admitted},
		{"no parent role", ordersProc, "parent", permission},
		{"no child role", ordersProc, "child", permission},
		{"no other permission", ordersProc, "reader", permission},
		{"first rule: kind and permission", staffProc, "staffreader", admitted},
		{"first rule: kind without permission", staffProc, "staff", permission},
		{"first rule: permission without kind", staffProc, "reader", permission},
		{"second rule: kind alone", staffProc, "service", admitted},
		{"kind alone", userProc, "nobody", admitted},
		{"another kind", userProc, "service", permission},
	} {
		t.Run(tt.name, func(t *testing.T) { s.want(t, tt.proc, as(tt.who), tt.want) })
	}
}

// A rule with a permission and no kind needs no registered kinds (the platform's admin surface relies on it), and
// the default admin role stands for platform.admin in a config built by hand.
func TestAuthz_PermissionWithoutKinds(t *testing.T) {
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	route := specRoute{specs: []connect.Spec{servertest.Spec(adminProc, servertest.Rules(rule("", server.PlatformAdminPermission)))}}
	srv, err := server.New(config.HTTP{Addr: ":0"}, config.Auth{Mode: config.AuthRequired, APIKeys: apiKeys()},
		slog.New(slog.NewTextHandler(io.Discard, nil)), []server.Route{route}, nil, nil, tel, nil, server.OIDCAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.PermissionRoles(); len(got) != 1 || got[server.PlatformAdminPermission] != config.DefaultAdminRole {
		t.Fatalf("PermissionRoles = %v", got)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	for who, status := range map[string]int{"literaladmin": http.StatusOK, "nobody": http.StatusForbidden} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+adminProc, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(server.APIKeyHeader, keyFor(who))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != status {
			t.Errorf("%s: status %d, want %d", who, res.StatusCode, status)
		}
	}
}

// An identity bound to no account, on a method the resolver lets it into: it passes only by its kind, is refused
// with permission_denied (not unauthenticated), and holds no permission.
func TestAuthz_UnboundIdentity(t *testing.T) {
	unbound := &fakeExternal{err: server.ErrUnboundIdentity, unbind: map[string]bool{openProc: true, ordersProc: true, userProc: true, claimProc: true, claimRWProc: true}}
	for _, mode := range []string{config.AuthOptional, config.AuthRequired} {
		for _, tt := range []struct {
			name      string
			claimants bool
			proc      string
			want      outcomeWant
		}{
			{"no kind: a kind rule", false, userProc, permission},
			{"no kind: a permission rule", false, ordersProc, permission},
			{"no kind: the claimant rule", false, claimProc, permission},
			{"no kind: public", false, openProc, admitted},
			{"claimant: its rule", true, claimProc, admitted},
			{"claimant: its rule with a permission", true, claimRWProc, permission},
			{"claimant: another kind's rule", true, userProc, permission},
		} {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				s := startAuthz(t, config.HTTP{Addr: ":0"}, config.Auth{Mode: mode},
					server.OIDCAuth{Verifier: &fakeVerifier{}, Resolver: unbound}, authzOptions(roleKind(tt.claimants))...)
				s.want(t, tt.proc, bearer(oidcToken), tt.want)
				if tt.proc == claimProc && tt.want == admitted && s.kind != "claimant" {
					t.Errorf("kind in the context = %q, want claimant", s.kind)
				}
			})
		}
	}
}

// A classifier answering a kind nobody registered meets no rule, a permission rule included, and is logged.
func TestAuthz_UnregisteredKindFromClassifier(t *testing.T) {
	s := startAuthz(t, config.HTTP{Addr: ":0"}, config.Auth{Mode: config.AuthRequired, APIKeys: apiKeys()}, server.OIDCAuth{}, authzOptions(roleKind(false))...)
	s.want(t, ordersProc, as("orders"), admitted)
	const logged = "unregistered principal kind"
	if strings.Contains(s.logs.String(), logged) {
		t.Fatalf("logged before any bad answer: %s", s.logs.String())
	}
	s.want(t, ordersProc, as("ghost"), permission) // it holds orders.admin
	out := s.logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, logged) || !strings.Contains(out, "kind=ghost") {
		t.Fatalf("want an error log naming the kind, got: %s", out)
	}
}

// Authorization runs outside the timeout: the classifier has no deadline, the handler does.
func TestAuthz_OutsideTheTimeout(t *testing.T) {
	classifierDeadline := false
	classify := func(c context.Context) string {
		_, classifierDeadline = c.Deadline()

		return "user"
	}
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	handlerDeadline := false
	s := &authzServer{}
	route := specRoute{specs: authzSpecs(), ran: func(c context.Context) {
		s.calls++
		_, handlerDeadline = c.Deadline()
	}}
	srv, err := server.New(config.HTTP{Addr: ":0", Timeout: time.Minute}, config.Auth{Mode: config.AuthRequired, APIKeys: apiKeys()},
		slog.New(slog.NewTextHandler(io.Discard, nil)), []server.Route{route}, nil, nil, tel, nil, server.OIDCAuth{}, authzOptions(classify)...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	s.url = ts.URL
	s.want(t, userProc, as("nobody"), admitted)
	if classifierDeadline || !handlerDeadline {
		t.Fatalf("deadline: classifier %v (want false), handler %v (want true)", classifierDeadline, handlerDeadline)
	}
}

func TestStartup_Declarations(t *testing.T) {
	const proc = "/fx.v1.Bad/Call"
	unreadable := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: proc, Schema: "not a descriptor"}
	decl := func(d *commonv1.MethodAuthz) connect.Spec { return servertest.Spec(proc, d) }
	for _, tt := range []struct {
		name string
		spec connect.Spec
		want string
	}{
		{"schema of another kind", unreadable, "no protobuf method descriptor"},
		{"no declaration", decl(nil), "declares no"},
		{"neither public nor rules", decl(&commonv1.MethodAuthz{}), "neither public nor rules"},
		{"public false", decl(&commonv1.MethodAuthz{Policy: &commonv1.MethodAuthz_Public{}}), "public: false"},
		{"no rule", decl(servertest.Rules()), "no rule"},
		{"empty rule", decl(servertest.Rules(rule("", ""))), "neither a kind nor a permission"},
		{"empty rule after a good one", decl(servertest.Rules(rule("user", ""), rule("", ""))), "rule 1"},
		{"unregistered kind", decl(servertest.Rules(rule("robot", ""))), `kind "robot" is not registered`},
		{"kind that requires a permission", decl(servertest.Rules(rule("staff", ""))), "must be declared with a permission"},
		{"unregistered permission", decl(servertest.Rules(rule("", "orders.delete"))), `permission "orders.delete" is not registered`},
		{"unregistered permission beside a kind", decl(servertest.Rules(rule("user", "orders.delete"))), `permission "orders.delete" is not registered`},
	} {
		for _, mode := range []string{config.AuthOff, config.AuthOptional, config.AuthRequired} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				err := newWith(t, mode, []connect.Spec{tt.spec}, authzOptions(roleKind(false))...)
				if err == nil || !strings.Contains(err.Error(), proc) || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("server.New = %v, want a refusal naming %s and %q", err, proc, tt.want)
				}
			})
		}
	}
	t.Run("every failing procedure is named", func(t *testing.T) {
		err := newWith(t, config.AuthOff, []connect.Spec{servertest.Spec("/fx.v1.Bad/A", nil), servertest.Spec("/fx.v1.Bad/B", nil)})
		if err == nil || !strings.Contains(err.Error(), "/fx.v1.Bad/A") || !strings.Contains(err.Error(), "/fx.v1.Bad/B") {
			t.Fatalf("server.New = %v", err)
		}
	})
}

func TestStartup_Registrations(t *testing.T) {
	classify := roleKind(false)
	kind := func(name string) server.PrincipalKind { return server.PrincipalKind{Name: name} }
	type tc struct {
		name string
		opts []server.Option
		want string // "" = starts
	}
	tests := []tc{
		{"valid", []server.Option{server.WithPrincipalKinds(classify, kind("user")), server.WithPermissions("orders.admin", "a1.read_only2")}, ""},
		{"empty kind name", []server.Option{server.WithPrincipalKinds(classify, kind(""))}, "empty name"},
		{"repeated kind", []server.Option{server.WithPrincipalKinds(classify, kind("user"), kind("user"))}, "registered twice"},
		{"no classifier", []server.Option{server.WithPrincipalKinds(nil, kind("user"))}, "both a classifier and at least one kind"},
		{"no kind", []server.Option{server.WithPrincipalKinds(classify)}, "both a classifier and at least one kind"},
		{"two kind registrations", []server.Option{server.WithPrincipalKinds(classify, kind("a")), server.WithPrincipalKinds(classify, kind("b"))}, "registered twice"},
		{"repeated permission", []server.Option{server.WithPermissions("orders.admin", "orders.admin")}, "registered twice"},
		{"repeated across options", []server.Option{server.WithPermissions("orders.admin"), server.WithPermissions("orders.admin")}, "registered twice"},
		{"platform.admin again", []server.Option{server.WithPermissions(server.PlatformAdminPermission)}, "registered twice"},
	}
	for _, bad := range []string{"", "orders", "orders.", ".admin", "Orders.admin", "orders.Admin", "orders.admin.eu", "orders-eu.admin", "orders.read-only", "1orders.admin", "orders._admin", "orders.admin "} {
		tests = append(tests, tc{"format " + bad, []server.Option{server.WithPermissions(bad)}, "want <bc>.<function>"})
	}
	for _, tt := range tests {
		for _, mode := range []string{config.AuthOff, config.AuthRequired} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				err := newWith(t, mode, nil, tt.opts...)
				switch {
				case tt.want == "" && err != nil:
					t.Fatalf("server.New = %v, want it to start", err)
				case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
					t.Fatalf("server.New = %v, want a refusal containing %q", err, tt.want)
				}
			})
		}
	}
}

// newWith is server.New over specs; only the error matters.
func newWith(t *testing.T, mode string, specs []connect.Spec, opts ...server.Option) error {
	t.Helper()
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	_, err = server.New(config.HTTP{Addr: ":0"}, config.Auth{Mode: mode, APIKeys: apiKeys()},
		slog.New(slog.NewTextHandler(io.Discard, nil)), []server.Route{specRoute{specs: specs}}, nil, nil, tel, nil, server.OIDCAuth{}, opts...)

	return err
}
