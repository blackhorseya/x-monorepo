//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf).
func Initialize(cfg *config.AppConfig) (*server.Server, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "HTTP", "Auth", "Log", "Postgres", "OTel"),
		postgres.NewDB,
		server.New,
		otel.ProviderSet,
		NewMounts,
		NewRoutes,
		NewPoisonService,
		NewDocs,
		NewReadiness,
		NewOIDCAuth,
		TraceOptions,
		// new-bc:rpc-providers
		// Credentials beyond the config: none in the template (see NoCredentialResolver, NoExternalIdentityResolver).
		NoCredentialResolver,
		NoExternalIdentityResolver,
	)

	return nil, nil, nil
}
