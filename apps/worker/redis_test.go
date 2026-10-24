package main

import (
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// The default broker never touches redis.url: a URL nobody listens on must not
// stop the worker from starting (config.example.yaml names the compose-up Redis).
func TestProvideRedis_PostgresBrokerIgnoresURL(t *testing.T) {
	rdb, cleanup, err := ProvideRedis(
		config.Worker{Broker: config.BrokerPostgres},
		config.Redis{URL: "redis://127.0.0.1:1/0"},
	)
	if err != nil {
		t.Fatalf("ProvideRedis: %v", err)
	}
	defer cleanup()
	if rdb != nil {
		t.Fatal("expected no client with broker=postgres")
	}
}

// With broker=redis the URL is used, and used eagerly: a bad one is a startup
// error, not a surprise at the first XREAD.
func TestProvideRedis_RedisBrokerPings(t *testing.T) {
	_, _, err := ProvideRedis(
		config.Worker{Broker: config.BrokerRedis},
		config.Redis{URL: "redis://127.0.0.1:1/0"},
	)
	if err == nil {
		t.Fatal("expected a ping error for an unreachable redis.url")
	}
}
