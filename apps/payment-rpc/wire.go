//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf). The payment provider comes in as an argument: run builds
// it from payment.provider (payment.ProvidePaymentProvider) before anything else is opened.
func Initialize(cfg *config.AppConfig, provider port.PaymentProvider) (*server.Server, func(), error) {
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
		TraceOptions,
		payment.ProviderSet,
	)

	return nil, nil, nil
}
