package eventbus

import (
	"log/slog"

	"github.com/ThreeDotsLabs/watermill-redisstream/pkg/redisstream"
	"github.com/ThreeDotsLabs/watermill/message"
	goredis "github.com/redis/go-redis/v9"
)

// NewRedisPublisher writes a topic to the Redis Stream of the same name.
// It is NOT the outbox: use cases keep publishing into Postgres inside their
// transaction, and the worker's forwarder copies each outbox table into its
// stream (apps/worker). Streams are unbounded by default; a MAXLEN is a
// retention decision left to the deployment.
func NewRedisPublisher(client goredis.UniversalClient, l *slog.Logger) (message.Publisher, error) {
	return redisstream.NewPublisher(redisstream.PublisherConfig{Client: client}, NewLogger(l))
}

// NewRedisSubscriber reads topics as Redis Streams consumer groups: replicas
// sharing consumerGroup split the work (XREADGROUP), a different group gets
// every message again, from the start of the stream.
//
// Pool sizing: every Subscribe holds one connection in a blocking XREADGROUP
// (BlockTime, 100ms by default) for as long as the subscription lives, so the
// go-redis pool must be larger than the number of subscriptions plus the
// claim / ack traffic. The worker has one subscription per consumed topic
// (three today) against go-redis's default pool of 10 × GOMAXPROCS.
func NewRedisSubscriber(client goredis.UniversalClient, consumerGroup string, l *slog.Logger) (message.Subscriber, error) {
	return redisstream.NewSubscriber(redisstream.SubscriberConfig{
		Client:        client,
		ConsumerGroup: consumerGroup,
	}, NewLogger(l))
}
