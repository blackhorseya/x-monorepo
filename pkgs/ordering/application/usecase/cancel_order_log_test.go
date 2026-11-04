package usecase_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// CancelOrderInput.Reason is another BC's free text (inventory builds it from SKUs and
// durations): it is untrusted and unstable, so it must never reach a log line (#347, #427).
func TestCancelOrder_DoesNotLogUpstreamReason(t *testing.T) {
	const secret = "reservation r-9 for SKU-SECRET expired after 24h0m0s"

	var buf bytes.Buffer
	c := contextx.WithLogger(context.Background(), slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	repo := memory.NewOrderRepository()
	clock := fixedClock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	rec := &recorder{}
	create := usecase.NewCreateOrder(repo, memory.NewTransactor(), rec, fixedIDs{id: "o-log"}, clock, ownership.New("admin"), memory.NewFlatPriceList(500, "USD"))
	cancel := usecase.NewCancelOrder(repo, rec, clock, ownership.New("admin"))

	if _, err := create.Execute(c, dto.CreateOrderInput{CustomerID: "c-1", Items: []dto.OrderLine{{SKU: "SKU-1", Quantity: 1}}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := cancel.Execute(c, dto.CancelOrderInput{OrderID: "o-log", Reason: secret})
	if err != nil || out == nil || out.Status != "CANCELLED" {
		t.Fatalf("cancel = %+v, %v", out, err)
	}

	// Positive control: the use case does log the cancellation, so an empty buffer cannot pass.
	if !strings.Contains(buf.String(), "o-log") {
		t.Fatalf("cancel logged nothing about the order; the negative check below would be vacuous:\n%s", buf.String())
	}
	for _, leak := range []string{secret, "SKU-SECRET", "24h0m0s"} {
		if strings.Contains(buf.String(), leak) {
			t.Fatalf("log contains upstream reason text %q:\n%s", leak, buf.String())
		}
	}
}
