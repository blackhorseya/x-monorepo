package order_test

import (
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

func TestParseStatus_RoundTrip(t *testing.T) {
	for _, s := range []order.Status{order.StatusPending, order.StatusCancelled} {
		got, err := order.ParseStatus(s.String())
		if err != nil || got != s {
			t.Fatalf("ParseStatus(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := order.ParseStatus("UNKNOWN"); !errors.Is(err, order.ErrInvalidStatus) {
		t.Fatalf("unknown name: err = %v", err)
	}
	if _, err := order.ParseStatus(""); !errors.Is(err, order.ErrInvalidStatus) {
		t.Fatalf("empty name: err = %v", err)
	}
}
