//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/identity"
	identitycli "github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/cli"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	inventorycli "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/cli"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf).
func Initialize(cfg *config.AppConfig) (*Worker, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "Log", "Postgres", "Redis", "OTel", "Worker"),
		postgres.NewDB,
		ProvideRedis, // redis.NewClient only when worker.broker is redis
		otel.ProviderSet,
		usecasex.NewRecorder,
		eventbus.ProvidePublisher,
		ordering.ProviderSet,
		inventory.ProviderSet,
		NewWorker,
	)

	return nil, nil, nil
}

// InitializeExpire builds `worker expire`: inventory's reservation TTL sweep
// on the database, without the router. Same component configs as Initialize
// minus Redis / Worker (a sweep is one process reading and writing Postgres;
// its events go through the outbox like everyone else's).
func InitializeExpire(cfg *config.AppConfig) (*inventorycli.Expire, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "Log", "Postgres", "OTel"),
		postgres.NewDB,
		otel.ProviderSet,
		usecasex.NewRecorder,
		eventbus.ProvidePublisher,
		inventory.ProviderSet,
	)

	return nil, nil, nil
}

// InitializeBootstrap builds `worker bootstrap`: identity's first account +
// first key on an empty store. Identity publishes no events, so it is not in
// Initialize (the router) — only this operator command needs it.
func InitializeBootstrap(cfg *config.AppConfig) (*identitycli.Bootstrap, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "Log", "Postgres", "OTel"),
		postgres.NewDB,
		otel.ProviderSet,
		usecasex.NewRecorder,
		identity.ProviderSet,
	)

	return nil, nil, nil
}
