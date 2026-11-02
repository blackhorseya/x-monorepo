// apps/payment-rpc is a composition root: load config → wire → mount the payment BC's rpc adapter →
// run. No business logic, no mapping.
//
// It is apps/catalog-rpc for the payment BC: the same server and auth interceptor, but only
// payment's routes, docs and Postgres probe. It has no identity BC, so no CredentialResolver and no
// OIDC: auth.api_keys are the only credentials it knows, and any other key is rejected (docs/payment.md
// §7). The schema is apps/migrate's; this binary never migrates.
//
// This slice has one subcommand's worth of behaviour: serve (StartPayment, GetPayment). consume,
// the webhook route and expire arrive with the saga (slice 3).
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	paymentrpc "github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/rpc"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewRoutes lists every mounted service: the payment's only.
func NewRoutes(payment *paymentrpc.PaymentService) []server.Route {
	return []server.Route{payment}
}

// NewReadiness lists what /api/readyz checks: the database when one is configured (nil db =
// in-memory repository, nothing to probe). The provider is deliberately NOT probed: a provider
// outage fails StartPayment with UNAVAILABLE, it must not take the pod out of rotation (and with it
// GetPayment). docs/payment.md §7 /api/readyz.
func NewReadiness(db *gorm.DB) server.Readiness {
	if db == nil {
		return nil
	}
	return server.Readiness{{Name: "postgres", Probe: func(c context.Context) error {
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		return sqlDB.PingContext(c)
	}}}
}

// NewDocs lists the payment's OpenAPI document (served only when APP_HTTP_DOCS=true).
func NewDocs() []server.Doc {
	return []server.Doc{{Name: "payment", Spec: paymentv1.OpenAPI}}
}

// NoCredentialResolver is the absence of a resolver, spelled out: there is no identity BC here, so
// the auth interceptor checks auth.api_keys alone and an X-API-Key the config does not list is
// Unauthenticated (fail closed).
func NoCredentialResolver() server.CredentialResolver { return nil }

// NoOIDC is the zero value of the external-login half of the auth interceptor. Bearer tokens are
// not accepted at all: NewAuth has already refused any auth.jwt and auth.oidc.enabled, so the
// interceptor holds no verifier for them.
func NoOIDC() server.OIDCAuth { return server.OIDCAuth{} }

// TraceOptions: payment-rpc continues the trace rpc started. Its callers are rpc alone (the Service
// is ClusterIP, and the interceptor accepts nothing but rpc's service key), so a traceparent is
// trusted here. A caller who reaches it directly could still choose its own trace_id; that only
// affects observability data, never authorization.
func TraceOptions() []server.Option { return []server.Option{server.TrustRemoteTrace()} }

// NewAuth is the only config.Auth in the graph: apps/payment-rpc accepts static auth.api_keys and
// nothing else, so it refuses to start on a legacy auth.jwt or auth.oidc.enabled (a valid token
// would pass the interceptor, and customer_id / admin are trusted only because the caller holds the
// service key) and, with auth on, on an empty auth.api_keys. This is the composition root's rule:
// platform's defaults, which apps/rpc relies on, stay as they are.
func NewAuth(cfg *config.AppConfig) (config.Auth, error) {
	auth := cfg.Auth
	if auth.JWT != (config.JWT{}) {
		return config.Auth{}, errors.New("payment-rpc: auth.jwt is not supported, only auth.api_keys; unset auth.jwt")
	}
	if auth.OIDC.Enabled {
		return config.Auth{}, errors.New("payment-rpc: auth.oidc is not supported, only auth.api_keys; set auth.oidc.enabled to false")
	}
	if (auth.Mode == config.AuthOptional || auth.Mode == config.AuthRequired) && auth.APIKeys == "" {
		return config.Auth{}, errors.New("payment-rpc: auth.mode is on but auth.api_keys is empty")
	}

	return auth, nil
}

// CheckDurable is --require-durable: judged on the FINAL effective settings (config.Load has
// already applied env over the file), it refuses what the chart cannot rule out by itself: an
// empty or blank postgres.url (an external Secret may hold an empty value; the BC would then fall
// back to an in-memory repository, readyz 200, payments lost on restart), any auth.mode but
// required (an env var can say off), and any auth.api_keys but the single service key "rpc".
// customer_id and admin in a request are believed only because the caller holds that key, so a
// second key (console, ops...) would let its holder claim any customer. Messages carry the
// setting's name and, for the mode and the key count, their values, never a URL or key.
func CheckDurable(cfg *config.AppConfig) error {
	if strings.TrimSpace(cfg.Postgres.URL) == "" {
		return errors.New("payment-rpc: --require-durable: postgres.url is empty; refusing to serve from memory")
	}
	if cfg.Auth.Mode != config.AuthRequired {
		return fmt.Errorf("payment-rpc: --require-durable: auth.mode is %q, want %q", cfg.Auth.Mode, config.AuthRequired)
	}

	if n, only := apiKeyNames(cfg.Auth.APIKeys); n != 1 || only != serviceKeyName {
		return fmt.Errorf("payment-rpc: --require-durable: auth.api_keys must hold exactly one key, named %q (found %d)", serviceKeyName, n)
	}

	return nil
}

// serviceKeyName is the one credential payment-rpc accepts: rpc's. It narrows the risk, it does not
// prove it away: a shared Secret holding only "rpc=..." passes.
const serviceKeyName = "rpc"

// apiKeyNames counts the entries of auth.api_keys ("name=key[:role|role],name=key", read the way
// platform/server reads it: split on ",", trim, skip blanks, the name is what precedes the first
// "=") and returns the name of the first one. Names only; a key is never touched.
func apiKeyNames(s string) (count int, first string) {
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, _, _ := strings.Cut(pair, "=")
		if count == 0 {
			first = strings.TrimSpace(name)
		}
		count++
	}

	return count, first
}
