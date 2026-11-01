package eventbus

import (
	"testing"
	"time"
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
