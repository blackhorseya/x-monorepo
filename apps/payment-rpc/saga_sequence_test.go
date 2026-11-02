package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// flakyGateway is a payment provider that cannot be reached while down is set: cancelling a checkout and
// asking about a payment both fail, and both are counted (the positive control that the failure was met).
type flakyGateway struct {
	*mock.Provider
	down    atomic.Bool
	cancels atomic.Int64
	queries atomic.Int64
}

func (f *flakyGateway) CancelCheckout(c context.Context, ref string) error {
	f.cancels.Add(1)
	if f.down.Load() {
		return errors.New("gateway unreachable")
	}

	return f.Provider.CancelCheckout(c, ref)
}

func (f *flakyGateway) GetPayment(c context.Context, ref string) (dto.ProviderPaymentOutput, error) {
	f.queries.Add(1)
	if f.down.Load() {
		return dto.ProviderPaymentOutput{}, errors.New("gateway unreachable")
	}

	return f.Provider.GetPayment(c, ref)
}

// proc is a real binary running against the test's database: stopped with SIGTERM when the test ends, its
// output kept for the failure message.
type proc struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

func buildWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "worker")
	build := exec.Command("go", "build", "-o", bin, "../worker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ../worker: %v\n%s", err, out)
	}

	return bin
}

func (e *consumeEnv) workerEnv(group string) []string {
	return append(os.Environ(),
		"APP_POSTGRES_URL="+e.pg.URL, "APP_LOG_LEVEL=error", "APP_OTEL_ENDPOINT=",
		"APP_ORDERING_REQUIRE_PAYMENT=true", "APP_WORKER_CONSUMER_GROUP="+group, "APP_WORKER_POLL_INTERVAL=20ms",
	)
}

// runWorker starts the worker binary as `worker` (no arguments) does in a cluster.
func (e *consumeEnv) runWorker(bin, group string) {
	e.t.Helper()
	p := &proc{cmd: exec.Command(bin), out: &bytes.Buffer{}}
	p.cmd.Env = e.workerEnv(group)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }() // fire-and-forget: joined through done in the cleanup below
	e.t.Cleanup(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = p.cmd.Process.Kill()
			e.t.Errorf("the worker did not stop within 20s of SIGTERM\n%s", p.out)
		}
		if e.t.Failed() {
			e.t.Logf("worker output:\n%s", p.out)
		}
	})
}

// workerExpire runs `worker expire --older-than 24h` once, the TTL sweep a CronJob runs.
func (e *consumeEnv) workerExpire(bin string) string {
	e.t.Helper()
	cmd := exec.Command(bin, "expire", "--older-than", "24h")
	cmd.Env = e.workerEnv("unused")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("worker expire: %v\n%s", err, out)
	}

	return string(out)
}

// docs/payment.md §9, "查詢持續失敗的完整序列" (S3B): the whole chain with the real binaries and the real
// tables, from the reservation TTL to the refund. Real entries: the `worker` binary (ordering's and
// inventory's handlers), `worker expire` (the TTL sweep), payment's own consume and expire (InitializeConsume,
// InitializeExpire). The 3A version of this story injects the OrderCancelled; here it comes from the sweep.
//
//  1. an order is CONFIRMED under require_payment=true (snapshot true); payment's consumer creates its payment
//     (T1); the customer opens the checkout and pays at the provider, whose webhook never arrives
//  2. the reservation is held past its TTL (stored timestamps moved back 25h — a fake clock across processes);
//     `worker expire` releases the stock and asks ordering to cancel; ordering cancels (OrderCancelled)
//  3. the payment consumer ends the payment (T7) while the gateway is down: the checkout cannot be cancelled,
//     the best-effort failure blocks nothing; the expire sweep cannot ask the gateway either and moves nothing
//  4. the gateway recovers: the sweep finds the money (T8), the payment is refunded (T10, T11) and ordering,
//     handed PaymentCaptured for an order it already cancelled, refuses it: CANCELLED stays CANCELLED
func TestSequence_TTLSweepCancelsAnOrderWhoseCustomerPaidWhileTheGatewayWasDown(t *testing.T) {
	bin := buildWorker(t)
	e := newConsumeEnv(t)
	c := context.Background()
	for _, m := range []postgres.Migration{inventorypg.Migration()} {
		if err := postgres.Up(c, e.pg, m); err != nil {
			t.Fatal(err)
		}
	}
	// Everything this test touches starts empty (and the cursors of earlier tests are gone with the tables).
	for _, q := range []string{
		"TRUNCATE ordering.orders CASCADE", "TRUNCATE inventory.reservations, inventory.stocks CASCADE", "TRUNCATE payment.payments",
		"TRUNCATE " + "ordering.events_v1_ordercancellationrequested, ordering.events_v1_ordercancellationrequested_offsets",
		"TRUNCATE inventory.events_v1_stockreserved, inventory.events_v1_stockreserved_offsets",
		"TRUNCATE ordering.events_v1_orderpaid, ordering.events_v1_orderpaid_offsets",
		"TRUNCATE ordering.inbox, inventory.inbox",
	} {
		if err := e.db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// 1. the order, its reservation and its stock, as ordering and inventory would have left them.
	now := time.Now().UTC().Truncate(time.Microsecond)
	usd, _ := kernel.NewMoney(250, "USD")
	li, err := order.NewLineItem("S", 2, usd)
	if err != nil {
		t.Fatal(err)
	}
	o, err := order.New(order.Params{ID: "ord-seq", CustomerID: owner, Items: []order.LineItem{li}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	orders := orderingpg.NewOrderRepository(e.db)
	if err = orders.Save(c, o); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO inventory.stocks (sku, on_hand, reserved, version, updated_at) VALUES ('S', 10, 2, 2, now())",
		"INSERT INTO inventory.reservations (order_id, status, created_at, updated_at) VALUES ('ord-seq', 'RESERVED', now(), now())",
		"INSERT INTO inventory.reservation_lines (order_id, position, sku, quantity, short) VALUES ('ord-seq', 0, 'S', 2, false)",
	} {
		if err = e.db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	gateway := &flakyGateway{Provider: e.prov}
	e.start(gateway)
	e.runWorker(bin, e.group+"-w")
	e.publish("inventory.v1.StockReserved", &inventoryv1.StockReserved{OrderId: "ord-seq"})
	orderStatus := func() order.Status {
		got, ferr := orders.FindByID(c, "ord-seq")
		if ferr != nil {
			t.Fatal(ferr)
		}

		return got.Status()
	}
	e.eventually("ordering to CONFIRM the order", func() bool { return orderStatus() == order.StatusConfirmed })
	if got, _ := orders.FindByID(c, "ord-seq"); !got.PaymentRequired() {
		t.Fatal("the worker confirmed the order with the snapshot false although require_payment=true")
	}
	e.eventually("payment to create the payment for ord-seq (T1)", func() bool { _, perr := e.payment("ord-seq"); return perr == nil })

	// the customer opens the checkout (T2) and pays at the provider; no webhook reaches us
	p := e.mustPayment("ord-seq")
	co, err := e.prov.CreateCheckout(c, dtoCheckout(p))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.AttachCheckout(e.prov.Name(), co.Ref, co.URL, co.ExpiresAt, now); err != nil {
		t.Fatal(err)
	}
	if err = e.repo().Save(c, p); err != nil {
		t.Fatal(err)
	}
	if _, err = e.prov.Pay(co.Ref, p.Amount()); err != nil {
		t.Fatal(err)
	}

	// 2. the reservation outlives its TTL; the sweep runs; the gateway is down from here on
	gateway.down.Store(true)
	if err = e.db.Exec("UPDATE inventory.reservations SET created_at = now() - interval '25 hours', updated_at = now() - interval '25 hours' WHERE order_id = 'ord-seq'").Error; err != nil {
		t.Fatal(err)
	}
	if out := e.workerExpire(bin); !strings.Contains(out, "ord-seq") || !strings.Contains(out, "expired 1 reservation(s)") {
		t.Fatalf("worker expire output:\n%s", out)
	}
	e.eventually("ordering to cancel the order the sweep asked for", func() bool { return orderStatus() == order.StatusCancelled })

	// 3. T7 with the gateway down: the payment ends CANCELLED, the cancel was tried and failed
	e.eventually("payment to end ord-seq's payment (T7)", func() bool { return e.mustPayment("ord-seq").Status() == payment.StatusCancelled })
	if gateway.cancels.Load() == 0 {
		t.Fatal("positive control: the gateway was never asked to cancel the checkout")
	}
	for range 2 {
		if err = e.sweep(gateway); err != nil {
			t.Fatalf("a gateway that cannot be reached failed the sweep: %v", err)
		}
	}
	if gateway.queries.Load() == 0 {
		t.Fatal("positive control: the sweep never asked the gateway")
	}
	if got := e.mustPayment("ord-seq").Status(); got != payment.StatusCancelled {
		t.Fatalf("payment is %s after sweeps that could not reach the gateway, want CANCELLED (unchanged)", got)
	}
	if n := e.count(`SELECT count(*) FROM payment.events_v1_paymentrefundrequested`); n != 0 {
		t.Fatalf("%d refund requests while the gateway could not be asked", n)
	}

	// 4. recovery: the sweep finds the money, refunds it, and ordering refuses the late PaymentCaptured
	gateway.down.Store(false)
	if err = e.sweep(gateway); err != nil {
		t.Fatal(err)
	}
	e.eventually("the payment to be REFUNDED", func() bool { return e.mustPayment("ord-seq").Status() == payment.StatusRefunded })
	if n := e.prov.Refunds(co.Ref); n != 1 {
		t.Fatalf("the provider refunded %d times, want 1", n)
	}
	// ordering consumed payment.v1.PaymentCaptured (its group's cursor passed it) and left the order alone
	e.eventually("ordering's group to acknowledge PaymentCaptured", func() bool {
		return e.count(`SELECT count(*) FROM payment.events_v1_paymentcaptured t JOIN payment.events_v1_paymentcaptured_offsets o ON o.consumer_group = ? WHERE t."offset" <= o.offset_acked`, e.group+"-w.ordering") == 1
	})
	if got := orderStatus(); got != order.StatusCancelled {
		t.Fatalf("order is %s after a PaymentCaptured for a cancelled order, want CANCELLED", got)
	}
	if n := e.count(`SELECT count(*) FROM ordering.events_v1_orderpaid`); n != 0 {
		t.Fatalf("%d OrderPaid events for an order that was cancelled", n)
	}
	var reserved int
	if err = e.db.Raw("SELECT reserved FROM inventory.stocks WHERE sku = 'S'").Scan(&reserved).Error; err != nil || reserved != 0 {
		t.Fatalf("stock still reserved: %d, %v", reserved, err)
	}
}
