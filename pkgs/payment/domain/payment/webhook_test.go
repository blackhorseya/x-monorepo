package payment_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

func TestWebhookEvent(t *testing.T) {
	body := []byte(`{"id":"evt-1"}`)
	w, err := payment.NewWebhookEvent("mock", "evt-1", payment.WebhookCaptured, "ref-1", money(t, 1000, "TWD"), body, t0)
	if err != nil {
		t.Fatal(err)
	}
	if w.Provider() != "mock" || w.EventID() != "evt-1" || w.EventType() != payment.WebhookCaptured || w.Ref() != "ref-1" ||
		w.Amount() != money(t, 1000, "TWD") || !w.ReceivedAt().Equal(t0) || !w.IsCapture() || w.Processed() || !w.ProcessedAt().IsZero() {
		t.Fatalf("unexpected event: %+v", w)
	}
	// the body is the event's own copy, both ways
	body[0] = 'X'
	got := w.Body()
	if string(got) != `{"id":"evt-1"}` {
		t.Fatalf("body = %q: the event aliases the caller's slice", got)
	}
	got[0] = 'Y'
	if string(w.Body()) != `{"id":"evt-1"}` {
		t.Fatal("Body hands out the event's own slice")
	}

	w.MarkProcessed(t0.Add(time.Minute))
	w.MarkProcessed(t0.Add(time.Hour)) // the first time stands
	if !w.Processed() || !w.ProcessedAt().Equal(t0.Add(time.Minute)) {
		t.Fatalf("processed at %v, want the first mark", w.ProcessedAt())
	}

	other, err := payment.NewWebhookEvent("mock", "evt-2", "payment.refunded", "", kernel.Money{}, []byte("x"), t0)
	if err != nil || other.IsCapture() || other.Amount() != (kernel.Money{}) {
		t.Fatalf("an event without an amount: %+v %v", other, err)
	}
}

func TestNewWebhookEvent_Refusals(t *testing.T) {
	ok := []byte("x")
	long := strings.Repeat("a", 129)
	cases := map[string]func() (*payment.WebhookEvent, error){
		"no provider": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("", "e", "t", "r", kernel.Money{}, ok, t0)
		},
		"provider too long": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent(strings.Repeat("p", 65), "e", "t", "r", kernel.Money{}, ok, t0)
		},
		"no event id": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "", "t", "r", kernel.Money{}, ok, t0)
		},
		"event id too long": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", long, "t", "r", kernel.Money{}, ok, t0)
		},
		"no type": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "e", "", "r", kernel.Money{}, ok, t0)
		},
		"type too long": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "e", long, "r", kernel.Money{}, ok, t0)
		},
		"ref too long": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "e", "t", long, kernel.Money{}, ok, t0)
		},
		"no body": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "e", "t", "r", kernel.Money{}, nil, t0)
		},
		"body too large": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "e", "t", "r", kernel.Money{}, make([]byte, payment.MaxWebhookBody+1), t0)
		},
		"a negative amount": func() (*payment.WebhookEvent, error) {
			return payment.NewWebhookEvent("p", "e", "t", "r", money(t, 5, "TWD").Multiply(-1), ok, t0)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if w, err := build(); !errors.Is(err, payment.ErrInvalidWebhook) || w != nil {
				t.Fatalf("got %v, %v; want payment.invalid_webhook", w, err)
			}
		})
	}
	// the largest body is fine
	if _, err := payment.NewWebhookEvent("p", "e", "t", "r", kernel.Money{}, make([]byte, payment.MaxWebhookBody), t0); err != nil {
		t.Fatalf("a body of exactly the limit: %v", err)
	}
}

func TestReconstituteWebhook(t *testing.T) {
	w, err := payment.ReconstituteWebhook(payment.ReconstituteWebhookParams{
		Provider: "mock", EventID: "e", EventType: payment.WebhookCaptured, Ref: "r", Amount: money(t, 5, "TWD"),
		Body: []byte("b"), ReceivedAt: t0, ProcessedAt: t0.Add(time.Second),
	})
	if err != nil || !w.Processed() || !w.ProcessedAt().Equal(t0.Add(time.Second)) {
		t.Fatalf("reconstituted = %+v, %v", w, err)
	}
	if _, err = payment.ReconstituteWebhook(payment.ReconstituteWebhookParams{EventID: "e", EventType: "t", Body: []byte("b")}); !errors.Is(err, payment.ErrInvalidWebhook) {
		t.Fatalf("a row without a provider: %v", err)
	}
}
