package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

// Two mock instances that share a secret (two replicas, or one restarted) must never hand out the same
// checkout ref or notification id: the payment table's (provider, provider_ref) and the webhook log's
// (provider, event id) are keys across all of them. With a per-instance counter both payments stored
// "mock_co_1", and the second customer's genuine "payment.captured" was deduplicated as a redelivery
// of the first — its payment stayed PENDING.
func TestMockInstances_SharingASecretDoNotCollide(t *testing.T) {
	c := context.Background()
	clk := &clock{now: t0}
	secret := []byte("0123456789abcdef0123456789abcdef")
	repo, webhooks, rec, tx := memory.NewPaymentRepository(), memory.NewWebhookRepository(nil), &recorder{}, memory.NewTransactor()
	instance := func() *mock.Provider {
		return mock.New(mock.Options{Secret: secret, CheckoutTTL: 30 * time.Minute, Now: clk.Now})
	}
	a, b := instance(), instance()

	type leg struct {
		order string
		prov  *mock.Provider
	}
	legs := []leg{{"order-a", a}, {"order-b", b}}
	for _, l := range legs {
		p, err := payment.New(payment.NewParams{ID: payment.ID("pay-" + l.order), OrderID: l.order, CustomerID: owner, Total: twd(t, 1000), ExpiresAt: t0.Add(30 * time.Minute)}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if err = repo.Save(c, p); err != nil {
			t.Fatal(err)
		}
		if _, err = usecase.NewStartPayment(repo, l.prov, tx, clk).Execute(c, dto.StartPaymentInput{OrderID: l.order, CustomerID: owner}); err != nil {
			t.Fatalf("start %s: %v", l.order, err)
		}
	}

	pa, err := repo.FindByOrderID(c, "order-a")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := repo.FindByOrderID(c, "order-b")
	if err != nil {
		t.Fatal(err)
	}
	if pa.ProviderRef() == pb.ProviderRef() {
		t.Fatalf("two instances stored the same checkout ref %q", pa.ProviderRef())
	}

	// each customer pays on the page their own instance served; the notifications go through the
	// formal path (verify the signature, store, process) on a third instance that only shares the secret
	receiver := instance()
	for _, l := range legs {
		p, _ := repo.FindByOrderID(c, l.order)
		d, err := l.prov.Pay(p.ProviderRef(), twd(t, 1000))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = usecase.NewReceiveWebhook(webhooks, receiver, rec, tx, clk).Execute(c, dto.WebhookInput{Body: d.Body, Signature: d.Signature}); err != nil {
			t.Fatalf("receive %s: %v", l.order, err)
		}
		ev, err := receiver.VerifyWebhook(d.Body, d.Signature)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = usecase.NewProcessWebhook(repo, webhooks, rec, rec, clk).Execute(c, dto.WebhookReceivedInput{Provider: "mock", EventID: ev.ID}); err != nil {
			t.Fatalf("process %s: %v", l.order, err)
		}
	}

	for _, l := range legs {
		p, err := repo.FindByOrderID(c, l.order)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status() != payment.StatusCaptured {
			t.Errorf("%s is %s after its genuine payment.captured, want CAPTURED", l.order, p.Status())
		}
	}
}
