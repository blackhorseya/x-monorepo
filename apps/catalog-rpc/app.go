// apps/catalog-rpc is a composition root: load config → wire → mount the catalog
// BC's rpc adapter → run. No business logic, no mapping.
//
// It is apps/rpc cut down to one BC: the same server, the same auth
// interceptor, but only catalog's routes, docs and Postgres probe. It has no
// identity BC, so no CredentialResolver and no OIDC: auth.api_keys are the
// only credentials it knows, and any other key is rejected. The schema is
// apps/migrate's; this binary never migrates.
package main

import (
	"context"

	"gorm.io/gorm"

	catalogrpc "github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/rpc"
	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewRoutes lists every mounted service: the catalog's only.
func NewRoutes(catalog *catalogrpc.CatalogService) []server.Route {
	return []server.Route{catalog}
}

// NewReadiness lists what /api/readyz checks: the database when one is
// configured (nil db = in-memory repository, nothing to probe).
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

// NewDocs lists the catalog's OpenAPI document (served only when APP_HTTP_DOCS=true).
func NewDocs() []server.Doc {
	return []server.Doc{{Name: "catalog", Spec: catalogv1.OpenAPI}}
}

// NoCredentialResolver is the absence of a resolver, spelled out: there is no
// identity BC here, so the auth interceptor checks auth.api_keys alone and an
// X-API-Key the config does not list is Unauthenticated (fail closed).
func NoCredentialResolver() server.CredentialResolver { return nil }

// NoOIDC is the zero value of the external-login half of the auth interceptor:
// bearer tokens go to auth.jwt as before, auth.oidc is not read.
func NoOIDC() server.OIDCAuth { return server.OIDCAuth{} }
