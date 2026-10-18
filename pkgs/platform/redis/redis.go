// Package redis owns the shared Redis client, the way platform/postgres owns
// the gorm handle. Today only apps/worker uses it (platform/eventbus Redis
// Streams broker); repositories do not.
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// NewClient parses cfg.URL and pings once so misconfiguration fails at
// startup, not on the first message. The cleanup closes the pool. An empty
// URL returns a nil client, like postgres.NewDB: the app has no Redis, and
// whoever needs one (the worker with broker=redis) says so.
func NewClient(cfg config.Redis) (goredis.UniversalClient, func(), error) {
	if cfg.URL == "" {
		return nil, func() {}, nil
	}
	opts, err := goredis.ParseURL(cfg.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse redis.url: %w", err)
	}
	client := goredis.NewClient(opts)
	cleanup := func() { _ = client.Close() }

	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(c).Err(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("ping redis: %w", err)
	}

	return client, cleanup, nil
}
