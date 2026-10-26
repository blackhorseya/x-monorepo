package order_test

import (
	"errors"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func mustMoney(t *testing.T, amount int64, currency string) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(amount, currency)
	if err != nil {
		t.Fatalf("NewMoney: %v", err)
	}
	return m
}

func mustItem(t *testing.T, sku string, qty int32, price kernel.Money) order.LineItem {
	t.Helper()
	it, err := order.NewLineItem(sku, qty, price)
	if err != nil {
		t.Fatalf("NewLineItem: %v", err)
	}
	return it
}

func TestNewLineItem(t *testing.T) {
	usd := mustMoney(t, 250, "USD")
	tests := []struct {
		name    string
		sku     string
		qty     int32
		wantErr error
	}{
		{"ok", "SKU-1", 2, nil},
		{"empty sku", "", 1, order.ErrInvalidSKU},
		{"zero qty", "SKU-1", 0, order.ErrInvalidQuantity},
		{"negative qty", "SKU-1", -1, order.ErrInvalidQuantity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it, err := order.NewLineItem(tt.sku, tt.qty, usd)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if it.SKU() != tt.sku || it.Quantity() != tt.qty || it.UnitPrice() != usd {
				t.Fatalf("getters mismatch: %+v", it)
			}
			if it.Subtotal().Amount() != 500 {
				t.Fatalf("subtotal = %d, want 500", it.Subtotal().Amount())
			}
		})
	}
}

func TestNew(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	usd := mustMoney(t, 1000, "USD")
	twd := mustMoney(t, 300, "TWD")

	tests := []struct {
		name      string
		id        order.ID
		customer  string
		items     []order.LineItem
		wantErr   error
		wantTotal int64
	}{
		{name: "ok", id: "o-1", customer: "c-1", items: []order.LineItem{mustItem(t, "SKU-1", 2, usd), mustItem(t, "SKU-2", 1, usd)}, wantTotal: 3000},
		{name: "empty id", id: "", customer: "c-1", items: []order.LineItem{mustItem(t, "SKU-1", 1, usd)}, wantErr: order.ErrInvalidID},
		{name: "empty customer", id: "o-1", customer: "", items: []order.LineItem{mustItem(t, "SKU-1", 1, usd)}, wantErr: order.ErrEmptyCustomer},
		{name: "no items", id: "o-1", customer: "c-1", wantErr: order.ErrNoItems},
		{name: "mixed currency", id: "o-1", customer: "c-1", items: []order.LineItem{mustItem(t, "SKU-1", 1, usd), mustItem(t, "SKU-2", 1, twd)}, wantErr: kernel.ErrCurrencyMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := order.New(order.Params{ID: tt.id, CustomerID: tt.customer, Items: tt.items, Now: now})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got.ID() != tt.id || got.ID().String() != string(tt.id) || got.CustomerID() != tt.customer {
				t.Errorf("identity getters mismatch")
			}
			if got.Status() != order.StatusPending {
				t.Errorf("status = %v, want PENDING", got.Status())
			}
			if got.Version() != 0 {
				t.Errorf("version = %d, want 0 for a never-saved order", got.Version())
			}
			if got.Total().Amount() != tt.wantTotal {
				t.Errorf("total = %d, want %d", got.Total().Amount(), tt.wantTotal)
			}
			if !got.CreatedAt().Equal(now) || !got.UpdatedAt().Equal(now) {
				t.Errorf("timestamps not set to now")
			}
		})
	}
}

func TestReconstitute(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	items := []order.LineItem{mustItem(t, "SKU-1", 1, mustMoney(t, 100, "USD"))}

	o, err := order.Reconstitute(order.ReconstituteParams{
		ID: "o-1", CustomerID: "c-1", Items: items, Status: order.StatusCancelled, CreatedAt: created, UpdatedAt: updated, Version: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status() != order.StatusCancelled || !o.CreatedAt().Equal(created) || !o.UpdatedAt().Equal(updated) || o.Version() != 3 {
		t.Fatalf("reconstituted state mismatch: %+v", o)
	}

	if _, err = order.Reconstitute(order.ReconstituteParams{
		ID: "o-1", CustomerID: "c-1", Status: order.StatusPending, CreatedAt: created, UpdatedAt: updated,
	}); !errors.Is(err, order.ErrNoItems) {
		t.Fatalf("no items err = %v", err)
	}
}

func TestOrder_Cancel(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute)
	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{mustItem(t, "SKU-1", 1, mustMoney(t, 100, "USD"))}, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	if err := o.Cancel(later); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if o.Status() != order.StatusCancelled || !o.UpdatedAt().Equal(later) {
		t.Fatalf("status = %v, updatedAt = %v", o.Status(), o.UpdatedAt())
	}
	if err := o.Cancel(later); !errors.Is(err, order.ErrCannotCancel) {
		t.Fatalf("second cancel err = %v, want ErrCannotCancel", err)
	}
}

func TestOrder_Confirm(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute)
	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{mustItem(t, "SKU-1", 1, mustMoney(t, 100, "USD"))}, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	if err := o.Confirm(later); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if o.Status() != order.StatusConfirmed || !o.UpdatedAt().Equal(later) {
		t.Fatalf("status = %v, updatedAt = %v", o.Status(), o.UpdatedAt())
	}
	if err := o.Confirm(later); !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("second confirm err = %v, want ErrCannotConfirm", err)
	}
	// A confirmed order is still the customer's to cancel; inventory releases on OrderCancelled.
	if err := o.Cancel(later.Add(time.Minute)); err != nil {
		t.Fatalf("cancel after confirm: %v", err)
	}
	// A late StockReserved for a cancelled order is refused, not applied.
	if err := o.Confirm(later); !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("confirm after cancel err = %v, want ErrCannotConfirm", err)
	}
}

func TestOrder_Complete(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute)
	o, err := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{mustItem(t, "SKU-1", 1, mustMoney(t, 100, "USD"))}, Now: now})
	if err != nil {
		t.Fatal(err)
	}

	// Only a confirmed order can be completed: inventory has not reserved a pending one.
	if err := o.Complete(later); !errors.Is(err, order.ErrCannotComplete) {
		t.Fatalf("complete while pending err = %v, want ErrCannotComplete", err)
	}
	if err := o.Confirm(later); err != nil {
		t.Fatal(err)
	}
	if err := o.Complete(later.Add(time.Minute)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if o.Status() != order.StatusCompleted || !o.UpdatedAt().Equal(later.Add(time.Minute)) {
		t.Fatalf("status = %v, updatedAt = %v", o.Status(), o.UpdatedAt())
	}
	// Terminal: no second completion, no cancellation, no late confirmation.
	if err := o.Complete(later); !errors.Is(err, order.ErrCannotComplete) {
		t.Fatalf("second complete err = %v, want ErrCannotComplete", err)
	}
	if err := o.Cancel(later); !errors.Is(err, order.ErrCannotCancel) {
		t.Fatalf("cancel after complete err = %v, want ErrCannotCancel", err)
	}
	if err := o.Confirm(later); !errors.Is(err, order.ErrCannotConfirm) {
		t.Fatalf("confirm after complete err = %v, want ErrCannotConfirm", err)
	}
}

func TestOrder_ItemsIsACopy(t *testing.T) {
	o, _ := order.New(order.Params{ID: "o-1", CustomerID: "c-1", Items: []order.LineItem{mustItem(t, "SKU-1", 1, mustMoney(t, 100, "USD"))}, Now: time.Now()})
	items := o.Items()
	items[0] = order.LineItem{}
	if o.Items()[0].SKU() != "SKU-1" {
		t.Fatal("Items() leaked internal slice")
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		s           order.Status
		str         string
		canCancel   bool
		canConfirm  bool
		canComplete bool
	}{
		{order.StatusPending, "PENDING", true, true, false},
		{order.StatusConfirmed, "CONFIRMED", true, false, true},
		{order.StatusCompleted, "COMPLETED", false, false, false},
		{order.StatusCancelled, "CANCELLED", false, false, false},
		{order.StatusUnknown, "UNKNOWN", false, false, false},
		{order.Status(99), "UNKNOWN", false, false, false},
	}
	for _, tt := range tests {
		if tt.s.String() != tt.str {
			t.Errorf("%d.String() = %q, want %q", tt.s, tt.s.String(), tt.str)
		}
		if tt.s.CanTransitionTo(order.StatusCancelled) != tt.canCancel {
			t.Errorf("%s.CanTransitionTo(CANCELLED) = %v", tt.str, !tt.canCancel)
		}
		if tt.s.CanTransitionTo(order.StatusConfirmed) != tt.canConfirm {
			t.Errorf("%s.CanTransitionTo(CONFIRMED) = %v", tt.str, !tt.canConfirm)
		}
		if tt.s.CanTransitionTo(order.StatusCompleted) != tt.canComplete {
			t.Errorf("%s.CanTransitionTo(COMPLETED) = %v", tt.str, !tt.canComplete)
		}
		if tt.s.CanTransitionTo(order.StatusPending) {
			t.Errorf("%s must not transition to PENDING", tt.str)
		}
	}
}
