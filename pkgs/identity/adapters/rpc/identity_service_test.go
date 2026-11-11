package rpc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/identity"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// The whole BC behind a real server with auth required: a static admin key
// from the config creates an account and mints a key; that key (resolved
// through adapters/auth) then calls WhoAmI and, with the admin role, the
// admin procedures; a viewer key is PERMISSION_DENIED; revoking the key
// takes effect once the resolver cache is past its TTL.
func TestIdentityService_EndToEnd(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts := identity.ProvideAccountRepository(nil, logger)
	keys := identity.ProvideAPIKeyRepository(nil)
	tx := identity.ProvideTransactor(nil)
	clock := system.NewClock()
	ids := system.NewUUIDGenerator()
	secrets := system.NewSecretGenerator()
	svc := newService(accounts, keys, tx, clock, ids, secrets, rec, config.Auth{Mode: config.AuthRequired})
	// TTL 0: a revoke is visible on the next request, so the test does not sleep.
	resolver, err := auth.NewResolverWithTTL(identity.ProvideResolveAPIKey(accounts, keys, clock, rec), clock, 0, tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	// last_used_at on, as production wires it (auth.last_used_interval = 1m).
	resolver.TrackLastUsed(identity.ProvideTouchAPIKey(keys, clock, config.Auth{LastUsedInterval: time.Minute}, rec), time.Minute, logger)

	// ResolverTimeout as config.Load defaults it: the resolver path under test is the production one.
	cfg := config.Auth{Mode: config.AuthRequired, APIKeys: "root=root-key-0123456789abcdef:admin, viewer=viewer-key-0123456789abcdef", ResolverTimeout: 2 * time.Second}
	srv, err := server.New(config.HTTP{Addr: ":0"}, cfg, logger, []server.Route{svc}, nil, nil, tel, resolver, server.OIDCAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := func(key string) identityv1connect.IdentityServiceClient {
		return identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: keyTransport{key: key}}, ts.URL), servertest.WithErrorMeta()))
	}
	c := context.Background()
	root, viewer := client("root-key-0123456789abcdef"), client("viewer-key-0123456789abcdef")

	// Viewer: known, but not admin → PERMISSION_DENIED; WhoAmI needs no role.
	if _, err := viewer.ListAccounts(c, &identityv1.ListAccountsRequest{}); connect.CodeOf(err) != connect.CodePermissionDenied || errorCode(err) != server.PermissionDeniedCode {
		t.Fatalf("viewer list: %v", err)
	}
	me, err := viewer.WhoAmI(c, &identityv1.WhoAmIRequest{})
	if err != nil || me.GetAnonymous() || me.GetSubject() != "viewer" || me.GetMethod() != server.MethodAPIKey || me.GetCredentialId() != "" {
		t.Fatalf("viewer whoami = %+v, %v", me, err)
	}
	// Nobody: UNAUTHENTICATED.
	if _, err := client("").WhoAmI(c, &identityv1.WhoAmIRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous whoami: %v", err)
	}

	// Root creates an account with the admin role and a key for it.
	acc, err := root.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "ops bot", Roles: []string{"admin"}})
	if err != nil || acc.GetAccount().GetStatus() != identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		t.Fatalf("create account = %+v, %v", acc, err)
	}
	id := acc.GetAccount().GetId()
	if _, err := root.CreateAccount(c, &identityv1.CreateAccountRequest{Name: ""}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("validation: %v", err)
	}
	minted, err := root.CreateApiKey(c, &identityv1.CreateApiKeyRequest{AccountId: id, Name: "laptop"})
	if err != nil || minted.GetSecret() == "" || minted.GetApiKey().GetPrefix() != minted.GetSecret()[:8] || minted.GetApiKey().GetExpireTime() != nil {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
	if _, err := root.CreateApiKey(c, &identityv1.CreateApiKeyRequest{AccountId: id, Name: "past", ExpireTime: timestamppb.New(time.Now().Add(-time.Hour))}); connect.CodeOf(err) != connect.CodeInvalidArgument || errorCode(err) != "api_key.invalid_expiry" {
		t.Fatalf("past expiry: %v", err)
	}
	if _, err := root.CreateApiKey(c, &identityv1.CreateApiKeyRequest{AccountId: "nope", Name: "x"}); connect.CodeOf(err) != connect.CodeNotFound || errorCode(err) != "account.not_found" {
		t.Fatalf("unknown account: %v", err)
	}

	// The minted key is a principal: subject = account id, credential = key id, admin role → admin procedures work.
	bot := client(minted.GetSecret())
	me, err = bot.WhoAmI(c, &identityv1.WhoAmIRequest{})
	if err != nil || me.GetSubject() != id || me.GetName() != "ops bot" || !slices.Equal(me.GetRoles(), []string{"admin"}) || me.GetCredentialId() != minted.GetApiKey().GetId() {
		t.Fatalf("bot whoami = %+v, %v", me, err)
	}
	list, err := bot.ListAccounts(c, &identityv1.ListAccountsRequest{})
	if err != nil || len(list.GetAccounts()) != 1 {
		t.Fatalf("bot list = %+v, %v", list, err)
	}
	got, err := bot.GetAccount(c, &identityv1.GetAccountRequest{Id: id})
	if err != nil || got.GetAccount().GetName() != "ops bot" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// The bot's calls above (WhoAmI, ListAccounts, GetAccount) stamped last_use_time once
	// (one window); a key never used (minted.Msg's, before the first call) had none.
	if minted.GetApiKey().GetLastUseTime() != nil {
		t.Fatalf("a new key has no last_use_time: %v", minted.GetApiKey().GetLastUseTime())
	}
	resolver.Wait()
	keysOf, err := bot.ListApiKeys(c, &identityv1.ListApiKeysRequest{AccountId: id})
	if err != nil || len(keysOf.GetApiKeys()) != 1 || keysOf.GetApiKeys()[0].GetStatus() != identityv1.ApiKeyStatus_API_KEY_STATUS_ACTIVE {
		t.Fatalf("keys = %+v, %v", keysOf, err)
	}
	if lu := keysOf.GetApiKeys()[0].GetLastUseTime(); lu == nil || time.Since(lu.AsTime()) > time.Minute {
		t.Fatalf("last_use_time = %v, want just now", lu)
	}

	// A second DB admin, so the changes below are never about the last one. (The
	// static root key is a break-glass outside the guard: it does not count.)
	acc2, err := root.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "ops bot 2", Roles: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	id2 := acc2.GetAccount().GetId()
	minted2, err := root.CreateApiKey(c, &identityv1.CreateApiKeyRequest{AccountId: id2, Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}

	// Roles change → next request sees it (TTL 0 here; DefaultCacheTTL in production).
	if _, err := root.AssignRoles(c, &identityv1.AssignRolesRequest{Id: id, Roles: []string{"viewer"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.ListAccounts(c, &identityv1.ListAccountsRequest{}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("after role change: %v", err)
	}
	// Disable → the key is refused as invalid (not "disabled": the caller learns nothing).
	if _, err := root.DisableAccount(c, &identityv1.DisableAccountRequest{Id: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.WhoAmI(c, &identityv1.WhoAmIRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated || errorCode(err) != server.InvalidCredentialCode {
		t.Fatalf("disabled account: %v", err)
	}
	if _, err := root.DisableAccount(c, &identityv1.DisableAccountRequest{Id: id}); connect.CodeOf(err) != connect.CodeFailedPrecondition || errorCode(err) != "account.already_disabled" {
		t.Fatalf("disable twice: %v", err)
	}
	en, err := root.EnableAccount(c, &identityv1.EnableAccountRequest{Id: id})
	if err != nil || en.GetAccount().GetStatus() != identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		t.Fatalf("enable = %+v, %v", en, err)
	}
	if _, err := bot.WhoAmI(c, &identityv1.WhoAmIRequest{}); err != nil {
		t.Fatalf("re-enabled: %v", err)
	}
	// Revoke → refused for good; the listing still shows the key.
	rv, err := root.RevokeApiKey(c, &identityv1.RevokeApiKeyRequest{Id: minted.GetApiKey().GetId()})
	if err != nil || rv.GetApiKey().GetStatus() != identityv1.ApiKeyStatus_API_KEY_STATUS_REVOKED || rv.GetApiKey().GetRevokeTime() == nil {
		t.Fatalf("revoke = %+v, %v", rv, err)
	}
	if _, err := bot.WhoAmI(c, &identityv1.WhoAmIRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := root.RevokeApiKey(c, &identityv1.RevokeApiKeyRequest{Id: minted.GetApiKey().GetId()}); errorCode(err) != "api_key.already_revoked" {
		t.Fatalf("revoke twice: %v", err)
	}
	keysOf, _ = root.ListApiKeys(c, &identityv1.ListApiKeysRequest{AccountId: id})
	if len(keysOf.GetApiKeys()) != 1 || keysOf.GetApiKeys()[0].GetStatus() != identityv1.ApiKeyStatus_API_KEY_STATUS_REVOKED {
		t.Fatalf("keys after revoke = %+v", keysOf)
	}

	// The remaining DB admin cannot be locked out, by any of the three writes:
	// FAILED_PRECONDITION identity.last_admin, and nothing changed.
	lastAdmin := func(what string, err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || errorCode(err) != "identity.last_admin" {
			t.Fatalf("%s: code = %v / %q, err = %v", what, connect.CodeOf(err), errorCode(err), err)
		}
	}
	_, err = root.DisableAccount(c, &identityv1.DisableAccountRequest{Id: id2})
	lastAdmin("disable last admin", err)
	_, err = root.AssignRoles(c, &identityv1.AssignRolesRequest{Id: id2, Roles: []string{"viewer"}})
	lastAdmin("drop last admin role", err)
	_, err = root.RevokeApiKey(c, &identityv1.RevokeApiKeyRequest{Id: minted2.GetApiKey().GetId()})
	lastAdmin("revoke last admin key", err)
	if _, err := client(minted2.GetSecret()).ListAccounts(c, &identityv1.ListAccountsRequest{}); err != nil {
		t.Fatalf("the last admin still manages: %v", err)
	}
}

// With auth off every procedure is open and WhoAmI is anonymous.
func TestIdentityService_AuthOff(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	rec, _ := usecasex.NewRecorder(tel.Meter)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts := identity.ProvideAccountRepository(nil, logger)
	svc := newService(accounts, identity.ProvideAPIKeyRepository(nil), identity.ProvideTransactor(nil), system.NewClock(), system.NewUUIDGenerator(), system.NewSecretGenerator(), rec, config.Auth{})
	srv, err := server.New(config.HTTP{Addr: ":0"}, config.Auth{}, logger, []server.Route{svc}, nil, nil, tel, nil, server.OIDCAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL), servertest.WithErrorMeta()))
	c := context.Background()
	me, err := client.WhoAmI(c, &identityv1.WhoAmIRequest{})
	if err != nil || !me.GetAnonymous() {
		t.Fatalf("whoami = %+v, %v", me, err)
	}
	if _, err := client.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "x"}); err != nil {
		t.Fatalf("auth off create: %v", err)
	}
}

// newService assembles the service the way provider.go / wire do.
func newService(accounts account.Repository, keys apikey.Repository, tx port.Transactor, clock port.Clock, ids port.IDGenerator, secrets port.SecretGenerator, rec *usecasex.Recorder, auth config.Auth) *rpc.IdentityService {
	return newServiceWithSource(accounts, keys, tx, clock, ids, secrets, rec, auth, identity.ProvideExternalIdentitySource())
}

func newServiceWithSource(accounts account.Repository, keys apikey.Repository, tx port.Transactor, clock port.Clock, ids port.IDGenerator, secrets port.SecretGenerator, rec *usecasex.Recorder, auth config.Auth, src rpc.ExternalIdentitySource) *rpc.IdentityService {
	census := identity.ProvideAdminCensus(nil, accounts, keys, clock)
	invites := identity.ProvideInviteRepository(nil)
	audit := identity.ProvideAuditLog(nil)
	bindings := identity.ProvideExternalIdentityRepository(nil)

	return rpc.NewIdentityService(rpc.UseCases{
		CreateInvite:          identity.ProvideCreateInvite(accounts, invites, ids, audit, clock, tx, rec),
		RevokeSessions:        identity.ProvideRevokeSessions(accounts, audit, clock, tx, rec),
		ClaimInvite:           identity.ProvideClaimInvite(invites, bindings, accounts, clock, tx, rec),
		ListAccountIdentities: identity.ProvideListAccountIdentities(accounts, bindings, invites, clock, rec),
		ExternalIdentity:      src,
		CreateAccount:         identity.ProvideCreateAccount(accounts, ids, clock, tx, rec),
		GetAccount:            identity.ProvideGetAccount(accounts, rec),
		ListAccounts:          identity.ProvideListAccounts(accounts, rec),
		DisableAccount:        identity.ProvideDisableAccount(accounts, census, auth, clock, tx, rec),
		EnableAccount:         identity.ProvideEnableAccount(accounts, clock, tx, rec),
		AssignRoles:           identity.ProvideAssignRoles(accounts, census, auth, clock, tx, rec),
		CreateAPIKey:          identity.ProvideCreateAPIKey(accounts, keys, ids, secrets, clock, tx, rec),
		ListAPIKeys:           identity.ProvideListAPIKeys(accounts, keys, rec),
		RevokeAPIKey:          identity.ProvideRevokeAPIKey(keys, census, auth, clock, tx, rec),
		WhoAmI:                identity.ProvideWhoAmI(rec),
	}, auth)
}

type keyTransport struct{ key string }

func (t keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.key != "" {
		req.Header.Set(server.APIKeyHeader, t.key)
	}

	return http.DefaultTransport.RoundTrip(req)
}

// errorCode is the x-error-code of err: still on the error when a handler is called
// directly (server.ErrorMeta), response metadata when it came through a client (servertest.ErrorMeta).
func errorCode(err error) string {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return ""
	}
	if code := server.ErrorMeta(err).Get("x-error-code"); code != "" {
		return code
	}

	return servertest.ErrorMeta(err).Get("x-error-code")
}

// CreateInvite and RevokeSessions: admin only, and the answers carry the
// invite (no secret) and the revocation line.
func TestIdentityService_InviteAndRevokeSessions(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts := identity.ProvideAccountRepository(nil, logger)
	keys := identity.ProvideAPIKeyRepository(nil)
	clock := system.NewClock()
	svc := newService(accounts, keys, identity.ProvideTransactor(nil), clock, system.NewUUIDGenerator(), system.NewSecretGenerator(), rec, config.Auth{Mode: config.AuthRequired})
	cfg := config.Auth{Mode: config.AuthRequired, APIKeys: "root=root-key-0123456789abcdef:admin, viewer=viewer-key-0123456789abcdef", ResolverTimeout: 2 * time.Second}
	srv, err := server.New(config.HTTP{Addr: ":0"}, cfg, logger, []server.Route{svc}, nil, nil, tel, nil, server.OIDCAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := func(key string) identityv1connect.IdentityServiceClient {
		return identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: keyTransport{key: key}}, ts.URL), servertest.WithErrorMeta()))
	}
	c := context.Background()
	root, viewer := client("root-key-0123456789abcdef"), client("viewer-key-0123456789abcdef")

	acc, err := root.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "ops", Roles: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	id := acc.GetAccount().GetId()

	if _, err := viewer.CreateInvite(c, &identityv1.CreateInviteRequest{AccountId: id, Email: "ops@example.com"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer CreateInvite = %v", err)
	}
	if _, err := viewer.RevokeSessions(c, &identityv1.RevokeSessionsRequest{AccountId: id}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer RevokeSessions = %v", err)
	}

	inv, err := root.CreateInvite(c, &identityv1.CreateInviteRequest{AccountId: id, Email: "Ops@Example.com"})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	got := inv.GetInvite()
	if got.GetEmail() != "ops@example.com" || got.GetAccountId() != id || got.GetClaimTime() != nil ||
		!got.GetExpireTime().AsTime().Equal(got.GetCreateTime().AsTime().Add(7*24*time.Hour)) {
		t.Fatalf("CreateInvite = %+v", got)
	}
	if _, err := root.CreateInvite(c, &identityv1.CreateInviteRequest{AccountId: id, Email: "no-at-sign"}); connect.CodeOf(err) != connect.CodeInvalidArgument || errorCode(err) != "invite.invalid_email" {
		t.Fatalf("bad email = %v", err)
	}
	if _, err := root.CreateInvite(c, &identityv1.CreateInviteRequest{AccountId: "nope", Email: "a@b.co"}); connect.CodeOf(err) != connect.CodeNotFound || errorCode(err) != "account.not_found" {
		t.Fatalf("unknown account = %v", err)
	}

	rv, err := root.RevokeSessions(c, &identityv1.RevokeSessionsRequest{AccountId: id})
	if err != nil {
		t.Fatalf("RevokeSessions: %v", err)
	}
	a := rv.GetAccount()
	if a.GetSessionsRevokeTime() == nil || a.GetStatus() != identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE || !slices.Equal(a.GetRoles(), []string{"ops"}) {
		t.Fatalf("RevokeSessions = %+v", a)
	}
	if _, err := root.RevokeSessions(c, &identityv1.RevokeSessionsRequest{AccountId: "nope"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown account = %v", err)
	}
	// The revocation time shows on reads too.
	g, err := root.GetAccount(c, &identityv1.GetAccountRequest{Id: id})
	if err != nil || g.GetAccount().GetSessionsRevokeTime() == nil {
		t.Fatalf("GetAccount = %+v, %v", g, err)
	}
}

type fixedSource struct {
	id rpc.ExternalIdentity
	ok bool
}

func (s fixedSource) ExternalIdentityFrom(context.Context) (rpc.ExternalIdentity, bool) {
	return s.id, s.ok
}

// ClaimInvite is not admin-only and takes nothing from the client: the
// identity comes from the source (the verified token), and without one the
// call is UNAUTHENTICATED.
func TestIdentityService_ClaimInvite(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts := identity.ProvideAccountRepository(nil, logger)
	keys := identity.ProvideAPIKeyRepository(nil)
	tx := identity.ProvideTransactor(nil)
	clock := system.NewClock()
	ids, secrets := system.NewUUIDGenerator(), system.NewSecretGenerator()
	cfg := config.Auth{Mode: config.AuthOff}
	c := context.Background()

	// The default source (OIDC off): nobody has a verified identity.
	off := newService(accounts, keys, tx, clock, ids, secrets, rec, cfg)
	if _, err := off.ClaimInvite(c, &identityv1.ClaimInviteRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated || errorCode(err) != server.UnauthenticatedCode {
		t.Fatalf("no identity = %v", err)
	}

	src := &fixedSource{id: rpc.ExternalIdentity{Issuer: "https://idp.example", Subject: "sub-1", Email: "ops@example.com", EmailVerified: true}, ok: true}
	svc := newServiceWithSource(accounts, keys, tx, clock, ids, secrets, rec, cfg, src)
	acc, err := svc.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "ops", Roles: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateInvite(c, &identityv1.CreateInviteRequest{AccountId: acc.GetAccount().GetId(), Email: "ops@example.com"}); err != nil {
		t.Fatal(err)
	}

	// An unverified email is refused and does not spend the invite.
	src.id.EmailVerified = false
	if _, err := svc.ClaimInvite(c, &identityv1.ClaimInviteRequest{}); connect.CodeOf(err) != connect.CodeFailedPrecondition || errorCode(err) != "invite.email_unverified" {
		t.Fatalf("unverified = %v", err)
	}
	src.id.EmailVerified = true
	got, err := svc.ClaimInvite(c, &identityv1.ClaimInviteRequest{})
	if err != nil || got.GetAccount().GetId() != acc.GetAccount().GetId() {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	// The invite is spent: a second sub gets already_claimed.
	src.id.Subject = "sub-2"
	if _, err := svc.ClaimInvite(c, &identityv1.ClaimInviteRequest{}); connect.CodeOf(err) != connect.CodeFailedPrecondition || errorCode(err) != "invite.already_claimed" {
		t.Fatalf("second claim = %v", err)
	}
	// A source that has nothing (token absent) is UNAUTHENTICATED.
	src.ok = false
	if _, err := svc.ClaimInvite(c, &identityv1.ClaimInviteRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no token = %v", err)
	}
}

// ListAccountIdentities is admin-only, answers NOT_FOUND for an unknown
// account, shows bindings and invites with their status, and its response has
// no field that could carry a token, hash or secret.
func TestIdentityService_ListAccountIdentities(t *testing.T) {
	tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	defer telCleanup()
	rec, err := usecasex.NewRecorder(tel.Meter)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts := identity.ProvideAccountRepository(nil, logger)
	keys := identity.ProvideAPIKeyRepository(nil)
	src := &fixedSource{id: rpc.ExternalIdentity{Issuer: "https://idp.example", Subject: "sub-1", Email: "ops@example.com", EmailVerified: true}, ok: true}
	svc := newServiceWithSource(accounts, keys, identity.ProvideTransactor(nil), system.NewClock(), system.NewUUIDGenerator(), system.NewSecretGenerator(), rec, config.Auth{Mode: config.AuthRequired}, src)
	cfg := config.Auth{Mode: config.AuthRequired, APIKeys: "root=root-key-0123456789abcdef:admin, viewer=viewer-key-0123456789abcdef", ResolverTimeout: 2 * time.Second}
	srv, err := server.New(config.HTTP{Addr: ":0"}, cfg, logger, []server.Route{svc}, nil, nil, tel, nil, server.OIDCAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := func(key string) identityv1connect.IdentityServiceClient {
		return identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: keyTransport{key: key}}, ts.URL), servertest.WithErrorMeta()))
	}
	c := context.Background()
	root, viewer := client("root-key-0123456789abcdef"), client("viewer-key-0123456789abcdef")

	acc, err := root.CreateAccount(c, &identityv1.CreateAccountRequest{Name: "ops", Roles: []string{"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	id := acc.GetAccount().GetId()
	for _, email := range []string{"ops@example.com", "later@example.com"} {
		if _, err := root.CreateInvite(c, &identityv1.CreateInviteRequest{AccountId: id, Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	// Claim ops@example.com: the direct call stands in for the interceptor, which hands the handler the verified identity.
	if _, err := svc.ClaimInvite(c, &identityv1.ClaimInviteRequest{}); err != nil {
		t.Fatal(err)
	}

	// Not admin: PERMISSION_DENIED, however the account is named.
	if _, err := viewer.ListAccountIdentities(c, &identityv1.ListAccountIdentitiesRequest{AccountId: id}); connect.CodeOf(err) != connect.CodePermissionDenied || errorCode(err) != server.PermissionDeniedCode {
		t.Fatalf("viewer = %v", err)
	}
	if _, err := viewer.ListAccountIdentities(c, &identityv1.ListAccountIdentitiesRequest{AccountId: "nope"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer, unknown account = %v", err)
	}
	// No credential at all.
	anon := identityv1connect.NewIdentityServiceClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, ts.URL), servertest.WithErrorMeta()))
	if _, err := anon.ListAccountIdentities(c, &identityv1.ListAccountIdentitiesRequest{AccountId: id}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous = %v", err)
	}
	// Admin: unknown account is NOT_FOUND, an empty id fails validation.
	if _, err := root.ListAccountIdentities(c, &identityv1.ListAccountIdentitiesRequest{AccountId: "nope"}); connect.CodeOf(err) != connect.CodeNotFound || errorCode(err) != "account.not_found" {
		t.Fatalf("unknown account = %v", err)
	}
	if _, err := root.ListAccountIdentities(c, &identityv1.ListAccountIdentitiesRequest{}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty account id = %v", err)
	}

	got, err := root.ListAccountIdentities(c, &identityv1.ListAccountIdentitiesRequest{AccountId: id})
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	ids := got.GetIdentities()
	if len(ids) != 1 || ids[0].GetIssuer() != "https://idp.example" || ids[0].GetSubject() != "sub-1" || ids[0].GetAccountId() != id || ids[0].GetBindTime() == nil {
		t.Fatalf("identities = %+v", ids)
	}
	status := map[string]identityv1.InviteStatus{}
	for _, v := range got.GetInvites() {
		status[v.GetEmail()] = v.GetStatus()
		if v.GetAccountId() != id || v.GetExpireTime() == nil {
			t.Fatalf("invite = %+v", v)
		}
	}
	if len(status) != 2 || status["ops@example.com"] != identityv1.InviteStatus_INVITE_STATUS_CLAIMED || status["later@example.com"] != identityv1.InviteStatus_INVITE_STATUS_PENDING {
		t.Fatalf("invite statuses = %v", status)
	}
	for _, v := range got.GetInvites() {
		if (v.GetStatus() == identityv1.InviteStatus_INVITE_STATUS_CLAIMED) != (v.GetClaimTime() != nil) {
			t.Fatalf("claim_time does not follow status: %+v", v)
		}
	}

	// No secret, structurally: nothing in the response schema is named like a
	// credential, and the serialized response (a real one, with a claimed and a
	// pending invite) has no such key either.
	var walk func(protoreflect.MessageDescriptor, map[protoreflect.FullName]bool)
	walk = func(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		for i := range md.Fields().Len() {
			f := md.Fields().Get(i)
			for _, banned := range []string{"token", "hash", "secret"} {
				if strings.Contains(strings.ToLower(string(f.Name())), banned) {
					t.Errorf("%s.%s looks like a credential", md.FullName(), f.Name())
				}
			}
			if f.Message() != nil {
				walk(f.Message(), seen)
			}
		}
	}
	walk((&identityv1.ListAccountIdentitiesResponse{}).ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{})
	raw, err := protojson.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"token", "hash", "secret"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("response JSON mentions %q: %s", banned, raw)
		}
	}
}

// ClaimInvite learns who is calling from the identity the auth interceptor
// verified and put in the context — not from the request.
func TestContextExternalIdentity(t *testing.T) {
	src := rpc.ContextExternalIdentity{}
	if _, ok := src.ExternalIdentityFrom(context.Background()); ok {
		t.Fatal("a call without a verified OIDC token has no external identity")
	}
	c := contextx.WithExternalIdentity(context.Background(), contextx.ExternalIdentity{Issuer: "https://idp.example", Subject: "sub-1", Email: "a@example.com", EmailVerified: true})
	id, ok := src.ExternalIdentityFrom(c)
	if !ok || id.Issuer != "https://idp.example" || id.Subject != "sub-1" || id.Email != "a@example.com" || !id.EmailVerified {
		t.Fatalf("identity = %+v, %v", id, ok)
	}
}
