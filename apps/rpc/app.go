// apps/rpc is a composition root: load config → wire → mount each BC's rpc
// adapter → run. No business logic, no mapping.
//
// Adding a BC: add its *Service to NewRoutes and its ProviderSet to wire.go.
//
// Consumer adapters are NOT mounted here: apps/worker wires the same BC
// ProviderSets and runs their Subscriptions (scale by queue depth instead of
// RPS). Mounting a subscriber in this process would also be fine (go-micro
// style) — that is an apps/ decision, the BC does not care.
package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	catalogrpc "github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/rpc"
	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	identityrpc "github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/rpc"
	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewRoutes lists every mounted service: the BCs' and the platform's admin surface.
func NewRoutes(identity *identityrpc.IdentityService, catalog *catalogrpc.Forwarder, poison *admin.PoisonService) []server.Route {
	return []server.Route{identity, catalog, poison}
}

// NewPoisonService serves admin.v1.AdminService over the poison table of every
// BC that consumes events. Adding such a BC: add its Schema here (the worker's
// poison.go lists the same set). identity and catalog consume no events, so
// they have no poison table and none is listed yet.
func NewPoisonService(db *gorm.DB, auth config.Auth) *admin.PoisonService {
	return admin.NewPoisonService(db, auth, []string{})
}

// NewReadiness lists what /api/readyz checks: the database when one is
// configured (nil db = in-memory repositories, nothing to probe).
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

// NewDocs lists every BC's OpenAPI document (served only when APP_HTTP_DOCS=true).
func NewDocs() []server.Doc {
	return []server.Doc{
		{Name: "identity", Spec: identityv1.OpenAPI},
		{Name: "catalog", Spec: catalogv1.OpenAPI},
		{Name: "admin", Spec: adminv1.OpenAPI},
	}
}

// warmTimeout bounds the startup warm-up of the OIDC keys.
const warmTimeout = 15 * time.Second

// TraceOptions: rpc faces outside callers, so it does not continue an incoming traceparent (the server span is
// a new root with the caller's span as a link, and X-Trace-Id is rpc's own). Declared here, in code, and not
// configurable: no key, env var or Helm value can turn it on. catalog-rpc, whose only caller is rpc, trusts rpc's.
func TraceOptions() []server.Option { return []server.Option{server.DistrustRemoteTrace()} }

// NewOIDCAuth is the external-login half of the auth interceptor: with
// auth.oidc.enabled the multi-issuer verifier plus the identity BC's resolver,
// otherwise the zero value and bearer tokens go to auth.jwt as before. The
// verifier is built without network I/O; warming its keys runs in the
// background, so an IdP that is down does not stop rpc from starting.
func NewOIDCAuth(cfg config.Auth, ext server.ExternalIdentityResolver, mp metric.MeterProvider) (server.OIDCAuth, func(), error) {
	if !cfg.OIDC.Enabled {
		return server.OIDCAuth{}, func() {}, nil
	}
	v, err := oidc.New(cfg.OIDC, oidc.WithMeterProvider(mp))
	if err != nil {
		return server.OIDCAuth{}, nil, err
	}
	c, cancel := context.WithTimeout(context.Background(), warmTimeout)
	// fire-and-forget: best-effort key warm-up bounded by warmTimeout; the
	// verifier refetches on its own and Close (below) aborts it in flight.
	go func() {
		defer cancel()
		v.Warm(c)
	}()

	return server.OIDCAuth{Verifier: v, Resolver: ext}, func() { cancel(); v.Close() }, nil
}
