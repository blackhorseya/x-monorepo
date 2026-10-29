package order_test

import (
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// The three ways a priced order is refused before anything is written: all
// INVALID_ARGUMENT at the edge, each with its own stable x-error-code.
func TestPricingErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{order.ErrUnknownSKU, "order.unknown_sku"},
		{order.ErrInactiveSKU, "order.inactive_sku"},
		{order.ErrMixedCurrency, "order.mixed_currency"},
	} {
		var ke *kernel.Error
		if !errors.As(tc.err, &ke) || ke.Code() != tc.code || ke.Kind() != kernel.KindInvalidArgument {
			t.Errorf("%v: code/kind = %v, want %s / InvalidArgument", tc.err, ke, tc.code)
		}
	}
}
