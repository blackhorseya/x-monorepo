package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"

	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1/identityv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// External login through the whole wired server (docs/identity-login.md §6):
// a real OIDC verifier against a test IdP (discovery + JWKS on a loopback
// port), the identity BC's resolver and ClaimInvite handler, the real auth
// interceptor. The IdP is the only fake.
func TestAuth_OIDCLogin(t *testing.T) {
	const (
		client = "x-console"
		opsKey = "ops-key-0123456789abc"
	)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var idp *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": idp.URL, "jwks_uri": idp.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "OKP", "kid": "k1", "use": "sig", "alg": "EdDSA", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	idp = httptest.NewServer(mux)
	defer idp.Close()
	sign := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		tok.Header["kid"] = "k1"
		s, err := tok.SignedString(priv)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	srv, cleanup, err := initialize(t, &config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
		Auth: config.Auth{
			Mode:      config.AuthRequired,
			AdminRole: "admin",
			APIKeys:   "ops=" + opsKey + ":admin",
			OIDC: config.OIDC{Enabled: true, Issuers: []config.OIDCIssuer{
				{Issuer: idp.URL, Audience: client, AllowInsecureIssuer: true},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := context.Background()

	with := func(k, v string) connect.ClientOption {
		return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set(k, v)
				return next(c, req)
			}
		}))
	}
	admin := identityv1connect.NewIdentityServiceClient(http.DefaultClient, ts.URL, with(server.APIKeyHeader, opsKey))
	now := time.Now()
	alice := identityv1connect.NewIdentityServiceClient(http.DefaultClient, ts.URL, with(server.AuthorizationHeader, "Bearer "+sign(jwt.MapClaims{
		"iss": idp.URL, "sub": "alice-sub", "aud": client, "email": "alice@example.com", "email_verified": true,
		"roles": []string{"admin"}, // never believed: roles come from the account
		"iat":   now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})))

	created, err := admin.CreateAccount(c, connect.NewRequest(&identityv1.CreateAccountRequest{Name: "Alice", Roles: []string{"viewer"}}))
	if err != nil {
		t.Fatal(err)
	}
	accountID := created.Msg.GetAccount().GetId()
	if _, err := admin.CreateInvite(c, connect.NewRequest(&identityv1.CreateInviteRequest{AccountId: accountID, Email: "alice@example.com"})); err != nil {
		t.Fatal(err)
	}

	// Not bound yet: ClaimInvite only.
	_, err = alice.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{}))
	var ce *connect.Error
	if !errAs(err, &ce) || ce.Code() != connect.CodePermissionDenied || ce.Meta().Get("x-error-code") != server.UnboundIdentityCode {
		t.Fatalf("unbound WhoAmI = %v, want permission denied %s", err, server.UnboundIdentityCode)
	}
	// ListAccountIdentities is not on the unbound allow-list either: it never reaches the handler.
	_, err = alice.ListAccountIdentities(c, connect.NewRequest(&identityv1.ListAccountIdentitiesRequest{AccountId: accountID}))
	if !errAs(err, &ce) || ce.Code() != connect.CodePermissionDenied || ce.Meta().Get("x-error-code") != server.UnboundIdentityCode {
		t.Fatalf("unbound ListAccountIdentities = %v, want permission denied %s", err, server.UnboundIdentityCode)
	}
	claimed, err := alice.ClaimInvite(c, connect.NewRequest(&identityv1.ClaimInviteRequest{}))
	if err != nil || claimed.Msg.GetAccount().GetId() != accountID {
		t.Fatalf("ClaimInvite = %v, %v", claimed, err)
	}

	// Bound: the account's principal, with the account's roles and not the token's.
	who, err := alice.WhoAmI(c, connect.NewRequest(&identityv1.WhoAmIRequest{}))
	if err != nil {
		t.Fatalf("bound WhoAmI: %v", err)
	}
	if who.Msg.GetSubject() != accountID || who.Msg.GetMethod() != server.MethodJWT || len(who.Msg.GetRoles()) != 1 || who.Msg.GetRoles()[0] != "viewer" {
		t.Fatalf("WhoAmI = %+v, want the account's principal", who.Msg)
	}
	_, err = alice.ListAccounts(c, connect.NewRequest(&identityv1.ListAccountsRequest{}))
	if !errAs(err, &ce) || ce.Code() != connect.CodePermissionDenied || ce.Meta().Get("x-error-code") != server.PermissionDeniedCode {
		t.Fatalf("ListAccounts with the token's admin claim = %v, want permission denied %s", err, server.PermissionDeniedCode)
	}
	// Bound, but a viewer: the admin role is checked on the account's roles, not the token's.
	_, err = alice.ListAccountIdentities(c, connect.NewRequest(&identityv1.ListAccountIdentitiesRequest{AccountId: accountID}))
	if !errAs(err, &ce) || ce.Code() != connect.CodePermissionDenied || ce.Meta().Get("x-error-code") != server.PermissionDeniedCode {
		t.Fatalf("viewer ListAccountIdentities = %v, want permission denied %s", err, server.PermissionDeniedCode)
	}
	// An admin sees the binding the login just made, and the spent invite.
	listed, err := admin.ListAccountIdentities(c, connect.NewRequest(&identityv1.ListAccountIdentitiesRequest{AccountId: accountID}))
	if err != nil {
		t.Fatalf("admin ListAccountIdentities: %v", err)
	}
	ids, invites := listed.Msg.GetIdentities(), listed.Msg.GetInvites()
	if len(ids) != 1 || ids[0].GetIssuer() != idp.URL || ids[0].GetSubject() != "alice-sub" || len(invites) != 1 || invites[0].GetStatus() != identityv1.InviteStatus_INVITE_STATUS_CLAIMED {
		t.Fatalf("listed = %+v", listed.Msg)
	}
}

func errAs(err error, target **connect.Error) bool {
	return err != nil && errors.As(err, target)
}
