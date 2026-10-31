//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog"
	"github.com/blackhorseya/x-monorepo/pkgs/identity"
	identityauth "github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf).
func Initialize(cfg *config.AppConfig) (*server.Server, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "HTTP", "Auth", "Log", "Postgres", "OTel", "Catalog"),
		postgres.NewDB,
		server.New,
		otel.ProviderSet,
		usecasex.NewRecorder,
		eventbus.ProvidePublisher,
		NewRoutes,
		NewPoisonService,
		NewDocs,
		NewReadiness,
		NewOIDCAuth,
		ordering.ProviderSet,
		inventory.ProviderSet,
		identity.ProviderSet,
		// The catalog is its own process (apps/catalog-rpc): rpc asks it over Connect with its service key.
		// That client is the catalogv1connect.CatalogServiceHandler ordering's pricing and inventory's
		// product check ask, and the Forwarder serves catalog.v1 here on top of it.
		catalog.ClientProviderSet,
		// The auth interceptor asks identity for keys the config does not know.
		wire.Bind(new(server.CredentialResolver), new(*identityauth.Resolver)),
		// ...and for the people behind an OIDC token (auth.oidc).
		wire.Bind(new(server.ExternalIdentityResolver), new(*identityauth.ExternalCredentials)),
	)

	return nil, nil, nil
}
