package eventbus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ThreeDotsLabs/watermill-redisstream/pkg/redisstream"
	"github.com/ThreeDotsLabs/watermill/message"
	goredis "github.com/redis/go-redis/v9"
)

// NewRedisPublisher writes a topic to the Redis Stream of the same name.
// It is NOT the outbox: use cases keep publishing into Postgres inside their
// transaction, and the worker's forwarder copies each outbox table into its
// stream (apps/worker). Streams are unbounded by default; a MAXLEN is a
// retention decision left to the deployment.
//
// Close is a no-op: redisstream's Publisher.Close closes the client it was
// given, and client is shared (subscribers, lag gauges). The router closes a
// forwarder's publisher as soon as that handler stops, which with Draining
// is while other handlers may still be acking. platform/redis.NewClient's
// cleanup owns the client's lifetime.
func NewRedisPublisher(client goredis.UniversalClient, l *slog.Logger) (message.Publisher, error) {
	pub, err := redisstream.NewPublisher(redisstream.PublisherConfig{Client: client}, NewLogger(l))
	if err != nil {
		return nil, err
	}

	return keepClient{pub}, nil
}

// keepClient is a Publisher whose Close leaves the shared client open.
type keepClient struct{ message.Publisher }

func (keepClient) Close() error { return nil }

// NewRedisSubscriber reads topics as Redis Streams consumer groups: replicas
// sharing consumerGroup split the work (XREADGROUP); a different group gets
// every message again, from the start of the stream (FromBeginning) or only
// entries added after the group was created (FromLatest, XGROUP CREATE "$").
// The group is created lazily on Subscribe; EnsureRedisGroup creates it
// earlier when the moment of creation matters.
//
// Pool sizing: every Subscribe holds one connection in a blocking XREADGROUP
// (BlockTime, 100ms by default) for as long as the subscription lives, so the
// go-redis pool must be larger than the number of subscriptions plus the
// claim / ack traffic. The worker has one subscription per consumed topic
// (three today) against go-redis's default pool of 10 × GOMAXPROCS.
func NewRedisSubscriber(client goredis.UniversalClient, consumerGroup string, start Start, l *slog.Logger) (message.Subscriber, error) {
	return redisstream.NewSubscriber(redisstream.SubscriberConfig{
		Client:        client,
		ConsumerGroup: consumerGroup,
		OldestId:      redisStartID(start),
	}, NewLogger(l))
}

// EnsureRedisGroup creates consumerGroup on topic's stream now (and the
// stream, if missing) at the position start says, so that "latest" is
// measured from this call rather than from whenever Subscribe first runs.
// An existing group is left where it is.
func EnsureRedisGroup(c context.Context, client goredis.UniversalClient, topic, consumerGroup string, start Start) error {
	err := client.XGroupCreateMkStream(c, topic, consumerGroup, redisStartID(start)).Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("eventbus: create redis group %s on %s: %w", consumerGroup, topic, err)
	}
	return nil
}

func redisStartID(start Start) string {
	if start == FromLatest {
		return "$"
	}
	return "0"
}
