package main

import (
	goredis "github.com/redis/go-redis/v9"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/redis"
)

// ProvideRedis is the worker's Redis client: connected (and pinged) only when
// worker.broker is redis, nil otherwise. redis.url is in config.example.yaml
// for `task compose-up`; a worker on the default broker must not fail to start
// because that Redis is not running (CI has none).
func ProvideRedis(worker config.Worker, cfg config.Redis) (goredis.UniversalClient, func(), error) {
	if worker.Broker != config.BrokerRedis {
		return nil, func() {}, nil
	}
	return redis.NewClient(cfg)
}
