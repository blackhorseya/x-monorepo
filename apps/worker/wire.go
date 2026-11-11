//go:build wireinject

package main

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// Initialize is the injector. wire.go is the ONLY place AppConfig is mapped
// into component configs (via wire.FieldsOf).
func Initialize(cfg *config.AppConfig) (*Worker, func(), error) {
	wire.Build(
		wire.FieldsOf(new(*config.AppConfig), "Log", "Postgres", "Redis", "OTel", "Worker"),
		postgres.NewDB,
		ProvideRedis, // redis.NewClient only when worker.broker is redis
		otel.ProviderSet,
		eventbus.ProvidePublisher,
		// new-bc:worker-providers
		NewConsumers,
		NewWorker,
	)

	return nil, nil, nil
}

