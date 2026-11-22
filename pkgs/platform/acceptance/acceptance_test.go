package acceptance

import (
	"errors"
	"slices"
	"testing"
)

func TestEnvClose_LastRegisteredFirstAndJoinsErrors(t *testing.T) {
	var order []string
	boom := errors.New("boom")
	e := &Env{}
	e.Defer(func() error { order = append(order, "first"); return nil })
	e.Defer(func() error { order = append(order, "second"); return boom })

	if err := e.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want boom", err)
	}
	if want := []string{"second", "first"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if err := e.Close(); err != nil || len(order) != 2 {
		t.Fatalf("second Close ran again: %v, %v", err, order)
	}
}
