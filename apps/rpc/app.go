// apps/rpc is a composition root: load config → wire → mount each BC's rpc
// adapter → run. No business logic, no mapping.
//
// Adding a BC: its ProviderSet in wire.go and its RPC (a parameter and an element) in NewMounts.
//
// Consumer adapters are NOT mounted here: apps/worker wires the same BC
// ProviderSets and runs their Subscriptions (scale by queue depth instead of
// RPS). Mounting a subscriber in this process would also be fine (go-micro
// style) — that is an apps/ decision, the BC does not care.
package main

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/oidc"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewMounts lists every BC: what each one mounts. This is the one place to add
// a BC besides wire.go; NewRoutes, NewDocs and NewPoisonService read from it.
func NewMounts(
// new-bc:mount-param
) []server.Mount {
	return []server.Mount{
		// new-bc:mount-elem
	}
}

// NoCredentialResolver is the template's server.CredentialResolver: none. The auth interceptor then knows only the
// keys and the JWT in the config (auth.api_keys, auth.jwt). To manage credentials elsewhere (a BC's database),
// replace this provider in wire.go with one that returns your implementation. Returns a nil interface, never a typed nil.
func NoCredentialResolver() server.CredentialResolver { return nil }

// NoExternalIdentityResolver is the template's server.ExternalIdentityResolver: none. With auth.oidc.enabled the rpc
// refuses to start (NewOIDCAuth). To log people in through an external IdP, implement the interface (it maps a
// verified (issuer, subject) to the principal of your account) and replace this provider in wire.go.
func NoExternalIdentityResolver() server.ExternalIdentityResolver { return nil }

// NewRoutes lists every mounted service: the BCs' and the platform's admin surface.
func NewRoutes(mounts []server.Mount, poison *admin.PoisonService) []server.Route {
	var routes []server.Route
	for _, m := range mounts {
		routes = append(routes, m.Routes...)
	}

	return append(routes, poison)
}

// NewPoisonService serves admin.v1.AdminService over the poison table of every
// BC that consumes events (its Mount's PoisonSchema; the worker's poison.go
// lists the same set). The template has no BC, so none is listed yet.
func NewPoisonService(db *gorm.DB, auth config.Auth, mounts []server.Mount) *admin.PoisonService {
	schemas := []string{}
	for _, m := range mounts {
		if m.PoisonSchema != "" {
			schemas = append(schemas, m.PoisonSchema)
		}
	}

	return admin.NewPoisonService(db, auth, schemas)
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

// NewDocs lists every BC's OpenAPI document and the platform's (served only when APP_HTTP_DOCS=true).
func NewDocs(mounts []server.Mount) []server.Doc {
	var docs []server.Doc
	for _, m := range mounts {
		docs = append(docs, m.Docs...)
	}

	return append(docs, server.Doc{Name: "admin", Spec: adminv1.OpenAPI})
}

// warmTimeout bounds the startup warm-up of the OIDC keys.
const warmTimeout = 15 * time.Second

// TraceOptions: rpc faces outside callers, so it does not continue an incoming traceparent (the server span is
// a new root with the caller's span as a link, and X-Trace-Id is rpc's own). Declared here, in code, and not
// configurable: no key, env var or Helm value can turn it on. rpc has no downstream call, so there is nothing to forward a trace to (add that case back with the first one).
func TraceOptions() []server.Option { return []server.Option{server.DistrustRemoteTrace()} }

// errNoExternalIdentityResolver is the refusal when auth.oidc.enabled has nothing to map a verified identity to an
// account with. Fixed text: it carries no configured value.
var errNoExternalIdentityResolver = errors.New("auth.oidc.enabled needs a server.ExternalIdentityResolver and apps/rpc wires none: implement it and provide it in apps/rpc/wire.go in place of NoExternalIdentityResolver, or set auth.oidc.enabled to false")

// NewOIDCAuth is the external-login half of the auth interceptor: with
// auth.oidc.enabled the multi-issuer verifier plus the wired
// server.ExternalIdentityResolver, otherwise the zero value and bearer tokens
// go to auth.jwt as before. With auth.oidc.enabled and no resolver it refuses
// (whatever auth.mode is): a verified token would have no account to map to.
// The verifier is built without network I/O; warming its keys runs in the
// background, so an IdP that is down does not stop rpc from starting.
func NewOIDCAuth(cfg config.Auth, ext server.ExternalIdentityResolver, mp metric.MeterProvider) (server.OIDCAuth, func(), error) {
	if !cfg.OIDC.Enabled {
		return server.OIDCAuth{}, func() {}, nil
	}
	if ext == nil {
		return server.OIDCAuth{}, nil, errNoExternalIdentityResolver
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
