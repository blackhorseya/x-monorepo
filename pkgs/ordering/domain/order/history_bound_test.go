package order_test

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
)

// The two causes OrderCancellationRequested.cause adds (#427 slice 2): recorded as is, spelled as in
// the table of migration 0011, and a legal reason for the aggregate to be cancelled.
func TestCause_InventoryCancellationCauses(t *testing.T) {
	for cause, s := range map[order.Cause]string{
		order.CauseStockShortage:      "STOCK_SHORTAGE",
		order.CauseReservationExpired: "RESERVATION_EXPIRED",
	} {
		if cause.String() != s {
			t.Errorf("%d.String() = %q, want %q", cause, cause.String(), s)
		}
		got, err := order.ParseCause(s)
		if err != nil || got != cause {
			t.Errorf("ParseCause(%q) = %v, %v", s, got, err)
		}

		o := orderAt(t, order.StatusConfirmed, false)
		if err := o.Cancel(time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC), cause); err != nil {
			t.Errorf("Cancel(%s) = %v, want it accepted", s, err)
		} else if evs := o.PullEvents(); len(evs) != 1 || evs[0].Cause() != cause {
			t.Errorf("Cancel(%s) raised %v, want one event carrying that cause", s, evs)
		}
	}

	// Control: the causes that only the aggregate itself assigns are still refused for a Cancel.
	for _, cause := range []order.Cause{order.CausePaymentCaptured, order.CausePaymentExpired, order.CauseStockReserved} {
		o := orderAt(t, order.StatusConfirmed, false)
		if err := o.Cancel(time.Now(), cause); !errors.Is(err, order.ErrInvalidCause) {
			t.Errorf("Cancel(%s) = %v, want ErrInvalidCause", cause, err)
		}
	}
}

// GetOrderHistory is not paged because an order's history is bounded by the state machine. This
// computes the bound from CanTransitionTo (one CREATED entry, then the longest chain of transitions)
// and compares it with the number the proto promises; if the state machine ever allows a longer
// path, this turns red and the unpaged RPC has to be revisited.
func TestOrderHistory_LengthIsBoundedByTheStateMachine(t *testing.T) {
	var longest func(s order.Status, seen map[order.Status]bool) int
	longest = func(s order.Status, seen map[order.Status]bool) int {
		best := 0
		for _, next := range order.Statuses() {
			if !s.CanTransitionTo(next) {
				continue
			}
			if seen[next] {
				t.Fatalf("the state machine has a cycle through %s", next)
			}
			seen[next] = true
			if n := 1 + longest(next, seen); n > best {
				best = n
			}
			delete(seen, next)
		}

		return best
	}
	bound := 1 + longest(order.StatusPending, map[order.Status]bool{order.StatusPending: true})
	if bound < 2 {
		t.Fatalf("computed bound %d: the walk found no transitions, the check proves nothing", bound)
	}

	promised := func(file string, re *regexp.Regexp) int {
		t.Helper()
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s does not state the bound (%s)", file, re)
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatal(err)
		}

		return n
	}
	proto := "../../../../protos/ordering/v1/service.proto"
	if n := promised(proto, regexp.MustCompile(`bounds it at (\d+) entries`)); n != bound {
		t.Errorf("service.proto promises %d entries (rpc comment), the state machine allows %d", n, bound)
	}
	if n := promised(proto, regexp.MustCompile(`at most (\d+) entries`)); n != bound {
		t.Errorf("service.proto promises %d entries (response field), the state machine allows %d", n, bound)
	}
}
