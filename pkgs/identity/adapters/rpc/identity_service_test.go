package rpc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
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
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
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
	srv, err := server.New(config.HTTP{Addr: ":0"}, cfg, logger, []server.Route{svc}, nil, nil, tel, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	client := func(key string) identityv1connect.IdentityServiceClient {
		return identityv1connect.NewIdentityServiceClient(&http.Client{Transport: keyTransport{key: key}}, ts.URL)
	}
	c := context.Background()
	root, viewer := client("root-key-0123456789abcdef"), client("viewer-key-0123456789abcdef")

	// Viewer: known, but not admin → PERMISSION_DENIED; WhoAmI needs no role.
	if _, err := viewer.ListAccounts(c, connect.NewRequest(&identityv1.ListAccountsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied || errorCode(err) != server.PermissionDeniedCode {
		t.Fatalf("viewer list: %v", err)
	}
	me, err := viewer.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{}))
	if err != nil || me.Msg.GetAnonymous() || me.Msg.GetSubject() != "viewer" || me.Msg.GetMethod() != server.MethodAPIKey || me.Msg.GetCredentialId() != "" {
		t.Fatalf("viewer whoami = %+v, %v", me, err)
	}
	// Nobody: UNAUTHENTICATED.
	if _, err := client("").WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous whoami: %v", err)
	}

	// Root creates an account with the admin role and a key for it.
	acc, err := root.CreateAccount(c, connect.NewRequest(&identityv1.CreateAccountRequest{Name: "ops bot", Roles: []string{"admin"}}))
	if err != nil || acc.Msg.GetAccount().GetStatus() != identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		t.Fatalf("create account = %+v, %v", acc, err)
	}
	id := acc.Msg.GetAccount().GetId()
	if _, err := root.CreateAccount(c, connect.NewRequest(&identityv1.CreateAccountRequest{Name: ""})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("validation: %v", err)
	}
	minted, err := root.CreateApiKey(c, connect.NewRequest(&identityv1.CreateApiKeyRequest{AccountId: id, Name: "laptop"}))
	if err != nil || minted.Msg.GetSecret() == "" || minted.Msg.GetApiKey().GetPrefix() != minted.Msg.GetSecret()[:8] || minted.Msg.GetApiKey().GetExpireTime() != nil {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
	if _, err := root.CreateApiKey(c, connect.NewRequest(&identityv1.CreateApiKeyRequest{AccountId: id, Name: "past", ExpireTime: timestamppb.New(time.Now().Add(-time.Hour))})); connect.CodeOf(err) != connect.CodeInvalidArgument || errorCode(err) != "api_key.invalid_expiry" {
		t.Fatalf("past expiry: %v", err)
	}
	if _, err := root.CreateApiKey(c, connect.NewRequest(&identityv1.CreateApiKeyRequest{AccountId: "nope", Name: "x"})); connect.CodeOf(err) != connect.CodeNotFound || errorCode(err) != "account.not_found" {
		t.Fatalf("unknown account: %v", err)
	}

	// The minted key is a principal: subject = account id, credential = key id, admin role → admin procedures work.
	bot := client(minted.Msg.GetSecret())
	me, err = bot.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{}))
	if err != nil || me.Msg.GetSubject() != id || me.Msg.GetName() != "ops bot" || !slices.Equal(me.Msg.GetRoles(), []string{"admin"}) || me.Msg.GetCredentialId() != minted.Msg.GetApiKey().GetId() {
		t.Fatalf("bot whoami = %+v, %v", me, err)
	}
	list, err := bot.ListAccounts(c, connect.NewRequest(&identityv1.ListAccountsRequest{}))
	if err != nil || len(list.Msg.GetAccounts()) != 1 {
		t.Fatalf("bot list = %+v, %v", list, err)
	}
	got, err := bot.GetAccount(c, connect.NewRequest(&identityv1.GetAccountRequest{Id: id}))
	if err != nil || got.Msg.GetAccount().GetName() != "ops bot" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// The bot's calls above (WhoAmI, ListAccounts, GetAccount) stamped last_use_time once
	// (one window); a key never used (minted.Msg's, before the first call) had none.
	if minted.Msg.GetApiKey().GetLastUseTime() != nil {
		t.Fatalf("a new key has no last_use_time: %v", minted.Msg.GetApiKey().GetLastUseTime())
	}
	resolver.Wait()
	keysOf, err := bot.ListApiKeys(c, connect.NewRequest(&identityv1.ListApiKeysRequest{AccountId: id}))
	if err != nil || len(keysOf.Msg.GetApiKeys()) != 1 || keysOf.Msg.GetApiKeys()[0].GetStatus() != identityv1.ApiKeyStatus_API_KEY_STATUS_ACTIVE {
		t.Fatalf("keys = %+v, %v", keysOf, err)
	}
	if lu := keysOf.Msg.GetApiKeys()[0].GetLastUseTime(); lu == nil || time.Since(lu.AsTime()) > time.Minute {
		t.Fatalf("last_use_time = %v, want just now", lu)
	}

	// A second DB admin, so the changes below are never about the last one. (The
	// static root key is a break-glass outside the guard: it does not count.)
	acc2, err := root.CreateAccount(c, connect.NewRequest(&identityv1.CreateAccountRequest{Name: "ops bot 2", Roles: []string{"admin"}}))
	if err != nil {
		t.Fatal(err)
	}
	id2 := acc2.Msg.GetAccount().GetId()
	minted2, err := root.CreateApiKey(c, connect.NewRequest(&identityv1.CreateApiKeyRequest{AccountId: id2, Name: "laptop"}))
	if err != nil {
		t.Fatal(err)
	}

	// Roles change → next request sees it (TTL 0 here; DefaultCacheTTL in production).
	if _, err := root.AssignRoles(c, connect.NewRequest(&identityv1.AssignRolesRequest{Id: id, Roles: []string{"viewer"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.ListAccounts(c, connect.NewRequest(&identityv1.ListAccountsRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("after role change: %v", err)
	}
	// Disable → the key is refused as invalid (not "disabled": the caller learns nothing).
	if _, err := root.DisableAccount(c, connect.NewRequest(&identityv1.DisableAccountRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated || errorCode(err) != server.InvalidCredentialCode {
		t.Fatalf("disabled account: %v", err)
	}
	if _, err := root.DisableAccount(c, connect.NewRequest(&identityv1.DisableAccountRequest{Id: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition || errorCode(err) != "account.already_disabled" {
		t.Fatalf("disable twice: %v", err)
	}
	en, err := root.EnableAccount(c, connect.NewRequest(&identityv1.EnableAccountRequest{Id: id}))
	if err != nil || en.Msg.GetAccount().GetStatus() != identityv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		t.Fatalf("enable = %+v, %v", en, err)
	}
	if _, err := bot.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{})); err != nil {
		t.Fatalf("re-enabled: %v", err)
	}
	// Revoke → refused for good; the listing still shows the key.
	rv, err := root.RevokeApiKey(c, connect.NewRequest(&identityv1.RevokeApiKeyRequest{Id: minted.Msg.GetApiKey().GetId()}))
	if err != nil || rv.Msg.GetApiKey().GetStatus() != identityv1.ApiKeyStatus_API_KEY_STATUS_REVOKED || rv.Msg.GetApiKey().GetRevokeTime() == nil {
		t.Fatalf("revoke = %+v, %v", rv, err)
	}
	if _, err := bot.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := root.RevokeApiKey(c, connect.NewRequest(&identityv1.RevokeApiKeyRequest{Id: minted.Msg.GetApiKey().GetId()})); errorCode(err) != "api_key.already_revoked" {
		t.Fatalf("revoke twice: %v", err)
	}
	keysOf, _ = root.ListApiKeys(c, connect.NewRequest(&identityv1.ListApiKeysRequest{AccountId: id}))
	if len(keysOf.Msg.GetApiKeys()) != 1 || keysOf.Msg.GetApiKeys()[0].GetStatus() != identityv1.ApiKeyStatus_API_KEY_STATUS_REVOKED {
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
	_, err = root.DisableAccount(c, connect.NewRequest(&identityv1.DisableAccountRequest{Id: id2}))
	lastAdmin("disable last admin", err)
	_, err = root.AssignRoles(c, connect.NewRequest(&identityv1.AssignRolesRequest{Id: id2, Roles: []string{"viewer"}}))
	lastAdmin("drop last admin role", err)
	_, err = root.RevokeApiKey(c, connect.NewRequest(&identityv1.RevokeApiKeyRequest{Id: minted2.Msg.GetApiKey().GetId()}))
	lastAdmin("revoke last admin key", err)
	if _, err := client(minted2.Msg.GetSecret()).ListAccounts(c, connect.NewRequest(&identityv1.ListAccountsRequest{})); err != nil {
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
	srv, err := server.New(config.HTTP{Addr: ":0"}, config.Auth{}, logger, []server.Route{svc}, nil, nil, tel, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := identityv1connect.NewIdentityServiceClient(http.DefaultClient, ts.URL)
	c := context.Background()
	me, err := client.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{}))
	if err != nil || !me.Msg.GetAnonymous() {
		t.Fatalf("whoami = %+v, %v", me, err)
	}
	if _, err := client.CreateAccount(c, connect.NewRequest(&identityv1.CreateAccountRequest{Name: "x"})); err != nil {
		t.Fatalf("auth off create: %v", err)
	}
}

// newService assembles the service the way provider.go / wire do.
func newService(accounts account.Repository, keys apikey.Repository, tx port.Transactor, clock port.Clock, ids port.IDGenerator, secrets port.SecretGenerator, rec *usecasex.Recorder, auth config.Auth) *rpc.IdentityService {
	census := identity.ProvideAdminCensus(nil, accounts, keys)

	return rpc.NewIdentityService(rpc.UseCases{
		CreateAccount:  identity.ProvideCreateAccount(accounts, ids, clock, tx, rec),
		GetAccount:     identity.ProvideGetAccount(accounts, rec),
		ListAccounts:   identity.ProvideListAccounts(accounts, rec),
		DisableAccount: identity.ProvideDisableAccount(accounts, census, auth, clock, tx, rec),
		EnableAccount:  identity.ProvideEnableAccount(accounts, clock, tx, rec),
		AssignRoles:    identity.ProvideAssignRoles(accounts, census, auth, clock, tx, rec),
		CreateAPIKey:   identity.ProvideCreateAPIKey(accounts, keys, ids, secrets, clock, tx, rec),
		ListAPIKeys:    identity.ProvideListAPIKeys(accounts, keys, rec),
		RevokeAPIKey:   identity.ProvideRevokeAPIKey(keys, census, auth, clock, tx, rec),
		WhoAmI:         identity.ProvideWhoAmI(rec),
	}, auth)
}

type keyTransport struct{ key string }

func (t keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.key != "" {
		req.Header.Set(server.APIKeyHeader, t.key)
	}

	return http.DefaultTransport.RoundTrip(req)
}

func errorCode(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Meta().Get("x-error-code")
	}

	return ""
}
