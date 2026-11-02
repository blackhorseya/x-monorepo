package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

type webhookEnv struct {
	*sagaEnv
	webhooks *memory.WebhookRepository
}

func newWebhookEnv(t *testing.T) *webhookEnv {
	t.Helper()

	return &webhookEnv{sagaEnv: newSagaEnv(t, nil), webhooks: memory.NewWebhookRepository(nil)}
}

func (w *webhookEnv) receive() *usecase.ReceiveWebhook {
	return usecase.NewReceiveWebhook(w.webhooks, w.prov, w.rec, memory.NewTransactor(), w.clock)
}

func (w *webhookEnv) process() *usecase.ProcessWebhook {
	return usecase.NewProcessWebhook(w.repo, w.webhooks, w.rec, w.rec, w.clock)
}

func twd(t *testing.T, n int64) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(n, "TWD")
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// deliver is a webhook the provider sent: the signed body of a customer paying amount on the payment's checkout.
func (w *webhookEnv) paid(t *testing.T, order string, amount kernel.Money) mock.Delivery {
	t.Helper()
	p := w.payment(t, order)
	d, err := w.prov.Pay(p.ProviderRef(), amount)
	if err != nil {
		t.Fatal(err)
	}

	return d
}

// find returns the stored notification or fails the test (a missing one must not become a nil dereference).
func (w *webhookEnv) find(t *testing.T, id string) *payment.WebhookEvent {
	t.Helper()
	stored, err := w.webhooks.Find(context.Background(), "mock", id)
	if err != nil {
		t.Fatalf("notification %s: %v", id, err)
	}

	return stored
}

func (w *webhookEnv) accept(t *testing.T, d mock.Delivery) {
	t.Helper()
	if _, err := w.receive().Execute(context.Background(), dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
		t.Fatalf("receive: %v", err)
	}
	ev, err := w.prov.VerifyWebhook(d.Body, d.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.process().Execute(context.Background(), dto.WebhookReceivedInput{Provider: "mock", EventID: ev.ID}); err != nil {
		t.Fatalf("process: %v", err)
	}
}

// §7 Webhook 1–2: the signature is over the raw body and is checked before anything is written; a verified
// notification is stored and queued in one step; a redelivery stores and queues nothing.
func TestReceiveWebhook(t *testing.T) {
	c := context.Background()
	w := newWebhookEnv(t)
	w.seedPaid(t, orderID, true)
	d := w.paid(t, orderID, twd(t, 1000))

	if _, err := w.receive().Execute(c, dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
		t.Fatal(err)
	}
	if len(w.rec.webhooks) != 1 || !strings.HasPrefix(w.rec.webhooks[0], "mock/") {
		t.Fatalf("queued = %v, want one WebhookReceived", w.rec.webhooks)
	}
	ev, _ := w.prov.VerifyWebhook(d.Body, d.Signature)
	stored, err := w.webhooks.Find(c, "mock", ev.ID)
	if err != nil || !stored.IsCapture() || stored.Ref() != w.payment(t, orderID).ProviderRef() || stored.Amount() != twd(t, 1000) || string(stored.Body()) != string(d.Body) || stored.Processed() {
		t.Fatalf("stored = %+v, %v; want the raw body, the capture, unprocessed", stored, err)
	}

	// redelivery, any number of times: same answer, no second row, no second queue entry
	for range 3 {
		if _, err = w.receive().Execute(c, dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
	}
	if len(w.rec.webhooks) != 1 {
		t.Fatalf("queued %d times after redeliveries, want 1", len(w.rec.webhooks))
	}
}

func TestReceiveWebhook_Refusals(t *testing.T) {
	c := context.Background()
	w := newWebhookEnv(t)
	w.seedPaid(t, orderID, true)
	good := w.paid(t, orderID, twd(t, 1000))

	flipped := []byte(strings.Replace(string(good.Body), "1000", "1001", 1))
	reordered := []byte(`{"type":"payment.captured","id":"evt-x","ref":"r","amount":1000,"currency":"TWD"}`)
	for name, in := range map[string]dto.WebhookInput{
		"one byte changed":  {Body: flipped, Signature: good.Signature},
		"keys reordered":    {Body: reordered, Signature: good.Signature},
		"no signature":      {Body: good.Body},
		"a wrong signature": {Body: good.Body, Signature: "00" + good.Signature[2:]},
		"empty body":        {Signature: good.Signature},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.receive().Execute(c, in)
			if !errors.Is(err, port.ErrInvalidSignature) {
				t.Fatalf("err = %v, want an invalid signature", err)
			}
			if len(w.rec.webhooks) != 0 {
				t.Fatalf("queued %v for a notification that did not verify", w.rec.webhooks)
			}
			if _, err = w.webhooks.Find(c, "mock", "evt-x"); !errors.Is(err, payment.ErrNotFound) {
				t.Fatal("a notification that did not verify was stored")
			}
		})
	}

	t.Run("garbage is not stored either", func(t *testing.T) {
		if _, err := w.receive().Execute(c, dto.WebhookInput{Body: []byte("not json at all")}); !errors.Is(err, port.ErrInvalidSignature) {
			t.Fatalf("err = %v", err)
		}
		if len(w.rec.webhooks) != 0 {
			t.Fatal("garbage was queued")
		}
	})
}

// A body that verifies but is not a notification a payment can be told about is refused with the fixed
// sentinel, never with the provider's words, and nothing is stored.
func TestReceiveWebhook_VerifiedButUnusable(t *testing.T) {
	const marker = "SECRET-WEBHOOK-MARKER-8a13"
	c := context.Background()
	for name, verified := range map[string]func() (dto.ProviderWebhookOutput, error){
		"the provider cannot decode it": func() (dto.ProviderWebhookOutput, error) {
			return dto.ProviderWebhookOutput{}, errors.New("cannot decode " + marker)
		},
		"no event id": func() (dto.ProviderWebhookOutput, error) { return dto.ProviderWebhookOutput{Type: "t"}, nil },
		"an amount that is not money": func() (dto.ProviderWebhookOutput, error) {
			return dto.ProviderWebhookOutput{ID: "e", Type: "t", Amount: dto.MoneyOutput{Amount: -1, Currency: "TWD"}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWebhookEnv(t)
			uc := usecase.NewReceiveWebhook(w.webhooks, fixedVerify{Provider: w.prov, verified: verified}, w.rec, memory.NewTransactor(), w.clock)
			_, err := uc.Execute(c, dto.WebhookInput{Body: []byte("x"), Signature: "s"})
			if !errors.Is(err, port.ErrMalformedWebhook) || strings.Contains(err.Error(), marker) {
				t.Fatalf("err = %v, want only the malformed sentinel", err)
			}
			if len(w.rec.webhooks) != 0 {
				t.Fatalf("queued %v", w.rec.webhooks)
			}
		})
	}
}

// fixedVerify is a provider whose signature check always passes and whose decoding is verified().
type fixedVerify struct {
	*mock.Provider
	verified func() (dto.ProviderWebhookOutput, error)
}

func (f fixedVerify) VerifyWebhook([]byte, string) (dto.ProviderWebhookOutput, error) {
	return f.verified()
}

// ProcessWebhook: the D, D' and E columns of the §3 table through the use case, T3 / T4 / T8 / T9.
func TestProcessWebhook(t *testing.T) {
	c := context.Background()

	t.Run("T3: matching money captures the payment and publishes PaymentCaptured", func(t *testing.T) {
		w := newWebhookEnv(t)
		w.seedPaid(t, orderID, true)
		w.accept(t, w.paid(t, orderID, twd(t, 1000)))
		p := w.payment(t, orderID)
		if p.Status() != payment.StatusCaptured {
			t.Fatalf("status = %s", p.Status())
		}
		if ev := w.rec.events(); len(ev) != 1 || ev[0] != "PaymentCaptured:"+p.ID().String() {
			t.Fatalf("outbox = %v", ev)
		}
		if len(w.rec.raised()) != 0 {
			t.Fatalf("alerts = %v", w.rec.raised())
		}
	})

	t.Run("a processed notification is not applied twice", func(t *testing.T) {
		w := newWebhookEnv(t)
		w.seedPaid(t, orderID, true)
		d := w.paid(t, orderID, twd(t, 1000))
		w.accept(t, d)
		ev, _ := w.prov.VerifyWebhook(d.Body, d.Signature)
		before := w.payment(t, orderID)
		if _, err := w.process().Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: ev.ID}); err != nil {
			t.Fatal(err)
		}
		if after := w.payment(t, orderID); after.Version() != before.Version() || len(w.rec.events()) != 1 {
			t.Fatalf("applied twice: version %d -> %d, events %v", before.Version(), after.Version(), w.rec.events())
		}
	})

	t.Run("T4: a different amount, or currency, is NEEDS_REVIEW with an alert and no PaymentCaptured", func(t *testing.T) {
		for name, paid := range map[string]kernel.Money{"amount": twd(t, 999), "currency": mustMoney(t, 1000, "USD")} {
			w := newWebhookEnv(t)
			w.seedPaid(t, orderID, true)
			w.accept(t, w.paid(t, orderID, paid))
			if p := w.payment(t, orderID); p.Status() != payment.StatusNeedsReview {
				t.Fatalf("%s: status = %s, want NEEDS_REVIEW", name, p.Status())
			}
			if len(w.rec.events()) != 0 {
				t.Fatalf("%s: published %v for money that does not match", name, w.rec.events())
			}
			if got := w.rec.raised(); len(got) != 1 || got[0] != "amount_mismatch:"+orderID {
				t.Fatalf("%s: alerts = %v", name, got)
			}
		}
	})

	t.Run("T8 + T10: late money for an order that ended is captured and goes straight back", func(t *testing.T) {
		w := newWebhookEnv(t)
		w.seedPaid(t, orderID, true)
		d := w.paid(t, orderID, twd(t, 1000))                                                      // the customer paid
		if _, err := w.cancelled().Execute(c, dto.OrderEndedInput{OrderID: orderID}); err != nil { // ...but the order was cancelled first
			t.Fatal(err)
		}
		w.accept(t, d)
		p := w.payment(t, orderID)
		if p.Status() != payment.StatusRefunding {
			t.Fatalf("status = %s, want REFUNDING", p.Status())
		}
		ev := w.rec.events()
		if len(ev) != 2 || ev[0] != "PaymentCaptured:"+p.ID().String() || ev[1] != "PaymentRefundRequested:"+p.ID().String() {
			t.Fatalf("outbox = %v, want captured then refund requested", ev)
		}
	})

	t.Run("T9: late money that does not match after expiry is NEEDS_REVIEW", func(t *testing.T) {
		w := newWebhookEnv(t)
		w.seedPaid(t, orderID, true)
		p := w.payment(t, orderID)
		p.Unpaid(t0.Add(time.Hour))
		if err := w.repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		w.accept(t, w.paid(t, orderID, twd(t, 1)))
		if got := w.payment(t, orderID).Status(); got != payment.StatusNeedsReview {
			t.Fatalf("status = %s", got)
		}
	})

	t.Run("an orphan: money for a checkout no payment opened is alerted, then marked processed", func(t *testing.T) {
		w := newWebhookEnv(t)
		d := w.prov.Notify("evt-orphan", mock.WebhookCaptured, "mock_co_999", twd(t, 5))
		if _, err := w.receive().Execute(c, dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.process().Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: "evt-orphan"}); err != nil {
			t.Fatal(err)
		}
		if got := w.rec.raised(); len(got) != 1 || got[0] != "orphan_webhook:" {
			t.Fatalf("alerts = %v, want one orphan_webhook", got)
		}
		if !w.find(t, "evt-orphan").Processed() {
			t.Fatal("an orphan was left unprocessed: it would be retried for ever")
		}
	})

	t.Run("E: another event type is stored and ignored", func(t *testing.T) {
		w := newWebhookEnv(t)
		w.seedPaid(t, orderID, true)
		ref := w.payment(t, orderID).ProviderRef()
		d := w.prov.Notify("evt-other", "checkout.viewed", ref, twd(t, 1000))
		w.accept(t, d)
		if p := w.payment(t, orderID); p.Status() != payment.StatusPending {
			t.Fatalf("status = %s, an unrelated event moved the payment", p.Status())
		}
		if len(w.rec.events()) != 0 || len(w.rec.raised()) != 0 {
			t.Fatalf("events %v alerts %v", w.rec.events(), w.rec.raised())
		}
		if !w.find(t, "evt-other").Processed() {
			t.Fatal("an ignored event was left unprocessed")
		}
	})

	t.Run("an unknown notification", func(t *testing.T) {
		w := newWebhookEnv(t)
		if _, err := w.process().Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: "nope"}); !errors.Is(err, payment.ErrNotFound) {
			t.Fatalf("err = %v, want not found (a domain error: the consumer drops it)", err)
		}
	})

	t.Run("a payment that moved meanwhile is retried, not lost", func(t *testing.T) {
		w := newWebhookEnv(t)
		w.seedPaid(t, orderID, true)
		d := w.paid(t, orderID, twd(t, 1000))
		if _, err := w.receive().Execute(c, dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
			t.Fatal(err)
		}
		ev, _ := w.prov.VerifyWebhook(d.Body, d.Signature)
		uc := usecase.NewProcessWebhook(staleOnce{Repository: w.repo}, w.webhooks, w.rec, w.rec, w.clock)
		if _, err := uc.Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: ev.ID}); !errors.Is(err, payment.ErrStale) {
			t.Fatalf("err = %v, want ErrStale", err)
		}
		if w.find(t, ev.ID).Processed() {
			t.Fatal("a failed application marked the notification processed")
		}
		if _, err := w.process().Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: ev.ID}); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if got := w.payment(t, orderID).Status(); got != payment.StatusCaptured {
			t.Fatalf("status = %s after the retry", got)
		}
	})
}

func mustMoney(t *testing.T, n int64, cur string) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(n, cur)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// staleOnce makes the first Save of a payment lose the optimistic-lock race.
type staleOnce struct {
	payment.Repository
}

func (s staleOnce) Save(context.Context, *payment.Payment) error { return payment.ErrStale }
