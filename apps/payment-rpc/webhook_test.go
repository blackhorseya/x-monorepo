package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/webhook"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func postWebhook(t *testing.T, base string, body []byte, sig string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+webhook.Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if sig != "" {
		req.Header.Set(webhook.SignatureHeader, sig)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)

	return res.StatusCode, string(b)
}

// serveWebhook starts the real server (auth REQUIRED, one service key) with the env's mock behind it, so the
// test can both sign deliveries and watch what the route does to the database.
func (e *consumeEnv) serveWebhook() string {
	e.t.Helper()
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"}, Log: config.Log{Level: "error", Format: "text"}, Auth: requiredAuth, Postgres: e.pg,
	}, e.prov)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cleanup)
	ts := newTestServer(e.t, srv.Handler())

	return ts
}

// pendingWithCheckout seeds ord-1: a PENDING payment whose checkout is open at the env's mock provider.
func (e *consumeEnv) pendingWithCheckout() *payment.Payment {
	e.t.Helper()
	c := context.Background()
	total, err := kernel.NewMoney(1000, "TWD")
	if err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	p, err := payment.New(payment.NewParams{ID: payment.ID(mustID(e.t)), OrderID: "ord-1", CustomerID: owner, Total: total, ExpiresAt: now.Add(30 * time.Minute)}, now)
	if err != nil {
		e.t.Fatal(err)
	}
	co, err := e.prov.CreateCheckout(c, dtoCheckout(p))
	if err != nil {
		e.t.Fatal(err)
	}
	if err = p.AttachCheckout(e.prov.Name(), co.Ref, co.URL, co.ExpiresAt, now); err != nil {
		e.t.Fatal(err)
	}
	if err = e.repo().Save(c, p); err != nil {
		e.t.Fatal(err)
	}

	return e.mustPayment("ord-1")
}

func (e *consumeEnv) webhookRows() int { return e.count(`SELECT count(*) FROM payment.webhook_events`) }
func (e *consumeEnv) webhookQueued() int {
	return e.count(`SELECT count(*) FROM payment.events_v1_webhookreceived`)
}

// docs/payment.md §7 + §9「webhook route 不受 API key 影響」: with auth.mode required and no X-API-Key, a
// correctly signed notification is accepted (200), stored and queued once; one that does not verify — a
// changed byte, the same JSON with its keys reordered, a missing or wrong signature — is a 4xx and writes
// nothing. A redelivery is a 200 that adds nothing.
func TestWebhook_SignatureIsTheOnlyCredential(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.pendingWithCheckout()
	base := e.serveWebhook()
	d, err := e.prov.Pay(p.ProviderRef(), p.Amount())
	if err != nil {
		t.Fatal(err)
	}

	changed := bytes.Replace(d.Body, []byte("1000"), []byte("1001"), 1)
	reordered := []byte(`{"type":"payment.captured","id":"evt-x","ref":"` + p.ProviderRef() + `","amount":1000,"currency":"TWD"}`)
	for name, tc := range map[string]struct {
		body []byte
		sig  string
	}{
		"one byte changed":  {changed, d.Signature},
		"keys reordered":    {reordered, d.Signature},
		"no signature":      {d.Body, ""},
		"a wrong signature": {d.Body, strings.Repeat("0", len(d.Signature))},
	} {
		code, msg := postWebhook(t, base, tc.body, tc.sig)
		if code != http.StatusBadRequest || msg == "" {
			t.Fatalf("%s: %d %q, want a 400 with a fixed message", name, code, msg)
		}
	}
	if e.webhookRows() != 0 || e.webhookQueued() != 0 {
		t.Fatalf("the refused requests wrote %d rows and queued %d", e.webhookRows(), e.webhookQueued())
	}

	if code, _ := postWebhook(t, base, d.Body, d.Signature); code != http.StatusOK {
		t.Fatalf("a correctly signed notification: %d", code)
	}
	for range 3 {
		if code, _ := postWebhook(t, base, d.Body, d.Signature); code != http.StatusOK {
			t.Fatalf("a redelivery: %d, want 200 so the provider stops", code)
		}
	}
	if e.webhookRows() != 1 || e.webhookQueued() != 1 {
		t.Fatalf("after 4 deliveries: %d rows, %d queued; want 1 and 1", e.webhookRows(), e.webhookQueued())
	}
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusPending {
		t.Fatalf("status = %s: the route only stores, consume applies", got)
	}

	// the route is a POST only
	res, err := http.Get(base + webhook.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("GET on the webhook route answered 200")
	}
	// ...and bounded: a body over the limit is refused as too large, nothing stored
	if code, _ := postWebhook(t, base, make([]byte, payment.MaxWebhookBody+1), d.Signature); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the limit: %d", code)
	}
	if e.webhookRows() != 1 {
		t.Fatalf("rows = %d after the oversized request", e.webhookRows())
	}
}

// §9 "commit 後 consumer 掛掉再恢復" and the redelivery dedup, end to end: the route commits the notification
// and its queue entry while NO consumer is running; the payment is untouched; when the consumer comes up it
// converges to CAPTURED with exactly one PaymentCaptured; a redelivery after that changes nothing.
func TestWebhook_StoredWhileTheConsumerIsDownConvergesWhenItReturns(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.pendingWithCheckout()
	base := e.serveWebhook()
	d, _ := e.prov.Pay(p.ProviderRef(), p.Amount())

	if code, _ := postWebhook(t, base, d.Body, d.Signature); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	time.Sleep(300 * time.Millisecond) // a consumer that does not exist cannot have acted
	if got := e.mustPayment("ord-1").Status(); got != payment.StatusPending || e.webhookQueued() != 1 {
		t.Fatalf("status %s, queued %d; want PENDING and 1 while the consumer is down", got, e.webhookQueued())
	}

	e.start(nil)
	e.eventually("ord-1 to be CAPTURED", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusCaptured })
	e.eventually("the notification to be marked processed", func() bool {
		return e.count(`SELECT count(*) FROM payment.webhook_events WHERE processed_at IS NOT NULL`) == 1
	})
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentcaptured`); n != 1 {
		t.Fatalf("%d PaymentCaptured events, want 1", n)
	}

	// provider redelivers after the fact: 200, nothing new, nothing applied twice
	if code, _ := postWebhook(t, base, d.Body, d.Signature); code != http.StatusOK {
		t.Fatalf("late redelivery: %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentcaptured`); n != 1 || e.webhookRows() != 1 || e.webhookQueued() != 1 {
		t.Fatalf("after a late redelivery: %d captured, %d rows, %d queued; want 1, 1, 1", n, e.webhookRows(), e.webhookQueued())
	}
}

// T4 over the wire: money that does not match is NEEDS_REVIEW, not CAPTURED, and no PaymentCaptured leaves.
func TestWebhook_AMismatchIsNeedsReviewNotCaptured(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.pendingWithCheckout()
	base := e.serveWebhook()
	e.start(nil)
	short, _ := kernel.NewMoney(p.Amount().Amount()-1, "TWD")
	d, _ := e.prov.Pay(p.ProviderRef(), short)

	if code, _ := postWebhook(t, base, d.Body, d.Signature); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	e.eventually("ord-1 to be NEEDS_REVIEW", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusNeedsReview })
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentcaptured`); n != 0 {
		t.Fatalf("%d PaymentCaptured events for money that did not match", n)
	}
}

// T8 + T10 end to end: the order was cancelled, then the customer's payment lands: it is captured and refunded.
func TestWebhook_LateMoneyAfterCancellationIsRefunded(t *testing.T) {
	e := newConsumeEnv(t)
	p := e.pendingWithCheckout()
	base := e.serveWebhook()
	e.start(nil)
	d, _ := e.prov.Pay(p.ProviderRef(), p.Amount())

	e.publish(topicCancelled, orderCancelled("ord-1"))
	e.eventually("ord-1 to be CANCELLED", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusCancelled })
	if code, _ := postWebhook(t, base, d.Body, d.Signature); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	e.eventually("ord-1 to be REFUNDED", func() bool { return e.mustPayment("ord-1").Status() == payment.StatusRefunded })
	if got := e.prov.Refunds(p.ProviderRef()); got != 1 {
		t.Fatalf("refunds at the provider = %d, want 1", got)
	}
}

var _ = mock.Name

// webhookAnchors: what THIS rejected webhook must have left in each place we look.
var webhookAnchors = map[string]string{
	"response status line": "400",
	"response headers":     "Content-Type",
	"response body":        "invalid signature",
	"process log":          "payment.webhook_signature_invalid",
	"exported spans":       "/api/payment/webhook",
	"exported logs":        "payment.webhook_signature_invalid",
	"exported metrics":     "/api/payment/webhook",
	"/api/metrics":         "/api/payment/webhook",
}

// driveWebhook sends a notification whose body AND signature carry the marker, to the real route.
func driveWebhook(t *testing.T, base string, _ config.Postgres, prov *leakyProvider) exchange {
	t.Helper()
	body := []byte(`{"id":"evt-1","type":"payment.captured","ref":"` + leakMarker + `","amount":1,"currency":"TWD"}`)
	req, _ := http.NewRequest(http.MethodPost, base+webhook.Path, bytes.NewReader(body))
	req.Header.Set(webhook.SignatureHeader, "sig-"+leakMarker)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var hdr strings.Builder
	for k, vs := range res.Header {
		hdr.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("response = %d %q; want a 400", res.StatusCode, b)
	}

	return exchange{status: res.Status, headers: hdr.String(), body: string(b)}
}

// §9 敏感資料 for the webhook: a marker in the request body AND in the signature header, sent to the real route,
// is in none of the response, the process log, the exported spans / logs / metrics or /api/metrics. Positive
// control: the provider's verification was handed both. Every artifact must show THIS request (anchors).
func TestWebhook_BodyAndSignatureNeverLeave(t *testing.T) {
	artifacts, prov := leakScenario(t, false, driveWebhook)

	prov.mu.Lock()
	seen := strings.Join(prov.verified, "\n")
	prov.mu.Unlock()
	if strings.Count(seen, leakMarker) < 2 {
		t.Fatalf("positive control: the verification saw %q; the marker should have arrived in both the body and the signature", seen)
	}
	if got := unobserved(artifacts, webhookAnchors); len(got) != 0 {
		t.Fatalf("artifacts that do not show the rejected webhook, so cannot show the marker absent: %v", got)
	}
	if got := leaks(artifacts, leakMarker); len(got) != 0 {
		t.Fatalf("the request's body or signature reached: %v", got)
	}
}

// Fault injection for the scenario above: a collector that drops every export must turn the exported
// artifacts red, exactly those.
func TestWebhook_DroppedExportsAreNotMistakenForCleanOnes(t *testing.T) {
	artifacts, _ := leakScenario(t, true, driveWebhook)
	got := unobserved(artifacts, webhookAnchors)
	want := []string{"exported logs", "exported metrics", "exported spans"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unobserved = %v, want exactly %v", got, want)
	}
}
