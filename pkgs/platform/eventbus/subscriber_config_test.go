package eventbus

import (
	"testing"
	"time"

	wsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
)

func TestSubscriberConfig_PollInterval(t *testing.T) {
	if got := subscriberConfig("g", offsets()).PollInterval; got != 0 {
		t.Fatalf("no option: PollInterval = %s, want 0 (the library's default)", got)
	}
	cfg := subscriberConfig("g", offsets(), WithPollInterval(150*time.Millisecond))
	if cfg.PollInterval != 150*time.Millisecond {
		t.Fatalf("WithPollInterval: PollInterval = %s, want 150ms", cfg.PollInterval)
	}
	// Only the poll wait is tuned: the nack resend and the wait after a failed
	// query keep the library's defaults.
	if cfg.ResendInterval != 0 || cfg.RetryInterval != 0 || cfg.ConsumerGroup != "g" || cfg.InitializeSchema {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestSubscriberConfig_SubscribeBatchSize(t *testing.T) {
	size := func(opts ...SubscriberOption) int {
		t.Helper()
		sa, ok := subscriberConfig("g", offsets(), opts...).SchemaAdapter.(wsql.DefaultPostgreSQLSchema)
		if !ok {
			t.Fatal("SchemaAdapter is not the watermill-sql Postgres schema")
		}

		return sa.SubscribeBatchSize
	}
	if got := size(); got != 0 {
		t.Fatalf("no option: SubscribeBatchSize = %d, want 0 (the library's default)", got)
	}
	if got := size(WithSubscribeBatchSize(10)); got != 10 {
		t.Fatalf("WithSubscribeBatchSize(10): SubscribeBatchSize = %d, want 10", got)
	}
}
