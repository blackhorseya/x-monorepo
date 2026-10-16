package kernel_test

import (
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

func TestNewMoney(t *testing.T) {
	tests := []struct {
		name     string
		amount   int64
		currency string
		wantErr  error
	}{
		{"ok", 100, "USD", nil},
		{"negative", -1, "USD", kernel.ErrInvalidAmount},
		{"lowercase", 1, "usd", kernel.ErrInvalidCurrency},
		{"too short", 1, "US", kernel.ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := kernel.NewMoney(tt.amount, tt.currency)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestMoney_Add(t *testing.T) {
	usd, _ := kernel.NewMoney(100, "USD")
	twd, _ := kernel.NewMoney(100, "TWD")

	sum, err := usd.Add(usd)
	if err != nil || sum.Amount() != 200 {
		t.Fatalf("Add = %v, %v", sum, err)
	}
	if _, err = usd.Add(twd); !errors.Is(err, kernel.ErrCurrencyMismatch) {
		t.Fatalf("mismatch err = %v", err)
	}
}
