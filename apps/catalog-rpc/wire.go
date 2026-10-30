//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf).
func Initialize(cfg *config.AppConfig) (*server.Server, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "HTTP", "Log", "Postgres", "OTel"),
		postgres.NewDB,
		server.New,
		otel.ProviderSet,
		usecasex.NewRecorder,
		NewAuth,
		NewRoutes,
		NewDocs,
		NewReadiness,
		NoCredentialResolver,
		NoOIDC,
		catalog.ProviderSet,
	)

	return nil, nil, nil
}
