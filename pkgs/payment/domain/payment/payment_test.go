package payment_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
)

var (
	t0      = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	expires = t0.Add(30 * time.Minute)
	late    = expires.Add(time.Second)
)

func money(t *testing.T, amount int64, cur string) kernel.Money {
	t.Helper()
	m, err := kernel.NewMoney(amount, cur)
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// fixture rebuilds a payment in the given status the way a repository would, with the order
// outcome that status can only have and, optionally, a checkout the provider already opened.
func fixture(t *testing.T, st payment.Status, out payment.Outcome, withCheckout bool) *payment.Payment {
	t.Helper()
	p := payment.ReconstituteParams{
		ID: "pay-1", OrderID: "ord-1", CustomerID: "cust-1", Amount: money(t, 1000, "TWD"),
		Status: st, ExpiresAt: expires, Outcome: out, Version: 3, UpdatedAt: t0,
	}
	if withCheckout {
		p.Provider, p.ProviderRef, p.CheckoutURL, p.CheckoutExpiresAt = "mock", "ref-1", "https://pay.example/c/1", t0.Add(10*time.Minute)
	}
	if st == payment.StatusExpired || st == payment.StatusCancelled {
		p.TerminalAt = t0
	}
	x, err := payment.Reconstitute(p)
	if err != nil {
		t.Fatalf("fixture %s/%s: %v", st, out, err)
	}

	return x
}

type result struct {
	status  payment.Status
	outcome payment.Outcome
	publish []payment.Event
	alert   payment.Alert
	cancel  bool
	err     error
}

// events are the columns of the table in docs/payment.md §3 that act on an existing payment. B and C
// at "∅" and D/E/F at "∅" are application matters (tombstone, orphan webhook, NOT_FOUND), not the aggregate's.
var events = []struct {
	name string
	do   func(t *testing.T, x *payment.Payment) (payment.Effect, error)
}{
	{"A OrderConfirmed again", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.DuplicateConfirmed(true)
	}},
	{"A' OrderConfirmed again without total", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.DuplicateConfirmed(false)
	}},
	{"B OrderCancelled", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.OrderCancelled(t0), nil
	}},
	{"C OrderCompleted", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.OrderCompleted(t0), nil
	}},
	{"D webhook capture, amount matches", func(t *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Capture(payment.SourceWebhook, money(t, 1000, "TWD"), t0), nil
	}},
	{"D' webhook capture, amount differs", func(t *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Capture(payment.SourceWebhook, money(t, 999, "TWD"), t0), nil
	}},
	{"D' webhook capture, currency differs", func(t *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Capture(payment.SourceWebhook, money(t, 1000, "USD"), t0), nil
	}},
	{"F StartPayment, window open", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return payment.Effect{}, x.CheckStart(t0)
	}},
	{"F StartPayment, window closed", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return payment.Effect{}, x.CheckStart(late)
	}},
	{"G expire sweep, not due, provider has no money", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Unpaid(t0), nil
	}},
	{"G expire sweep, due, provider has no money", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Unpaid(late), nil
	}},
	{"G' sweep finds the money, amount matches", func(t *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Capture(payment.SourceReconcile, money(t, 1000, "TWD"), t0), nil
	}},
	{"G'' sweep finds money, amount differs", func(t *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.Capture(payment.SourceReconcile, money(t, 1, "TWD"), t0), nil
	}},
	{"H refund succeeded", func(_ *testing.T, x *payment.Payment) (payment.Effect, error) {
		return x.RefundSucceeded(t0)
	}},
}

var (
	pub    = func(e ...payment.Event) []payment.Event { return e }
	none   = payment.OutcomeNone
	cancOu = payment.OutcomeCancelled
	unpOu  = payment.OutcomeCompletedUnpaid
	P      = payment.StatusPending
	CAP    = payment.StatusCaptured
	EXP    = payment.StatusExpired
	CAN    = payment.StatusCancelled
	REFING = payment.StatusRefunding
	REFED  = payment.StatusRefunded
	REV    = payment.StatusNeedsReview
)

// The whole state × event table of docs/payment.md §3, every cell, including the "—" ones (an event
// the state can never receive still has one definite answer: a refusal, state untouched). The
// expectations are written out per cell, not derived from the implementation.
func TestTransitionTable(t *testing.T) {
	// rows: status → fixture outcome → event name → want. A cell not listed for a status means the
	// event leaves the payment exactly as it was with no effect and no error ("same").
	type cell = map[string]result
	same := func(st payment.Status, out payment.Outcome) result { return result{status: st, outcome: out} }
	a1 := func(st payment.Status, out payment.Outcome) result {
		return result{status: st, outcome: out, alert: payment.AlertMissingTotal, err: payment.ErrMissingTotal}
	}
	notPending := func(st payment.Status, out payment.Outcome) result {
		return result{status: st, outcome: out, err: payment.ErrNotPending}
	}
	notRefunding := func(st payment.Status, out payment.Outcome) result {
		return result{status: st, outcome: out, err: payment.ErrNotRefunding}
	}
	mismatch := func(st payment.Status, out payment.Outcome) result {
		return result{status: st, outcome: out, alert: payment.AlertAmountMismatch}
	}

	rows := []struct {
		name     string
		st       payment.Status
		out      payment.Outcome
		checkout bool
		want     cell
	}{
		{"PENDING, no checkout opened", P, none, false, cell{
			"B OrderCancelled":                           result{status: CAN, outcome: cancOu},
			"C OrderCompleted":                           result{status: CAN, outcome: unpOu},
			"D webhook capture, amount matches":          result{status: CAP, outcome: none, publish: pub(payment.EventCaptured)},
			"D' webhook capture, amount differs":         mismatch(REV, none),
			"D' webhook capture, currency differs":       mismatch(REV, none),
			"F StartPayment, window closed":              result{status: P, outcome: none, err: payment.ErrExpired},
			"G expire sweep, due, provider has no money": result{status: EXP, outcome: none, publish: pub(payment.EventExpired)},
			"G' sweep finds the money, amount matches":   result{status: CAP, outcome: none, publish: pub(payment.EventCaptured)},
			"G'' sweep finds money, amount differs":      mismatch(REV, none),
			"A' OrderConfirmed again without total":      a1(P, none),
			"H refund succeeded":                         notRefunding(P, none),
		}},
		{"PENDING, checkout opened", P, none, true, cell{
			"B OrderCancelled":                           result{status: CAN, outcome: cancOu, cancel: true},
			"C OrderCompleted":                           result{status: CAN, outcome: unpOu, cancel: true},
			"D webhook capture, amount matches":          result{status: CAP, outcome: none, publish: pub(payment.EventCaptured)},
			"D' webhook capture, amount differs":         mismatch(REV, none),
			"D' webhook capture, currency differs":       mismatch(REV, none),
			"F StartPayment, window closed":              result{status: P, outcome: none, err: payment.ErrExpired},
			"G expire sweep, due, provider has no money": result{status: EXP, outcome: none, publish: pub(payment.EventExpired)},
			"G' sweep finds the money, amount matches":   result{status: CAP, outcome: none, publish: pub(payment.EventCaptured)},
			"G'' sweep finds money, amount differs":      mismatch(REV, none),
			"A' OrderConfirmed again without total":      a1(P, none),
			"H refund succeeded":                         notRefunding(P, none),
		}},
		{"CAPTURED", CAP, none, true, cell{
			"B OrderCancelled": result{status: REFING, outcome: cancOu, publish: pub(payment.EventRefundRequested)},
			// the normal path: the order completed because of this payment
			"C OrderCompleted":                      same(CAP, none),
			"D' webhook capture, amount differs":    mismatch(CAP, none),
			"D' webhook capture, currency differs":  mismatch(CAP, none),
			"F StartPayment, window open":           notPending(CAP, none),
			"F StartPayment, window closed":         notPending(CAP, none),
			"A' OrderConfirmed again without total": a1(CAP, none),
			"H refund succeeded":                    notRefunding(CAP, none),
		}},
		{"EXPIRED", EXP, none, true, cell{
			"B OrderCancelled":                         result{status: EXP, outcome: cancOu},
			"C OrderCompleted":                         result{status: EXP, outcome: unpOu},
			"D webhook capture, amount matches":        result{status: CAP, outcome: none, publish: pub(payment.EventCaptured)},
			"D' webhook capture, amount differs":       mismatch(REV, none),
			"D' webhook capture, currency differs":     mismatch(REV, none),
			"F StartPayment, window open":              notPending(EXP, none),
			"F StartPayment, window closed":            notPending(EXP, none),
			"G' sweep finds the money, amount matches": result{status: CAP, outcome: none, publish: pub(payment.EventCaptured)},
			"G'' sweep finds money, amount differs":    mismatch(REV, none),
			"A' OrderConfirmed again without total":    a1(EXP, none),
			"H refund succeeded":                       notRefunding(EXP, none),
		}},
		// T8 → T10: the order had already ended, so the late money goes straight back.
		{"EXPIRED, the order was cancelled", EXP, cancOu, true, cell{
			"B OrderCancelled":                         same(EXP, cancOu),
			"C OrderCompleted":                         same(EXP, cancOu),
			"D webhook capture, amount matches":        result{status: REFING, outcome: cancOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"D' webhook capture, amount differs":       mismatch(REV, cancOu),
			"D' webhook capture, currency differs":     mismatch(REV, cancOu),
			"F StartPayment, window open":              notPending(EXP, cancOu),
			"F StartPayment, window closed":            notPending(EXP, cancOu),
			"G' sweep finds the money, amount matches": result{status: REFING, outcome: cancOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"G'' sweep finds money, amount differs":    mismatch(REV, cancOu),
			"A' OrderConfirmed again without total":    a1(EXP, cancOu),
			"H refund succeeded":                       notRefunding(EXP, cancOu),
		}},
		{"EXPIRED, the order was completed without it", EXP, unpOu, true, cell{
			"B OrderCancelled":                         same(EXP, unpOu),
			"C OrderCompleted":                         same(EXP, unpOu),
			"D webhook capture, amount matches":        result{status: REFING, outcome: unpOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"D' webhook capture, amount differs":       mismatch(REV, unpOu),
			"D' webhook capture, currency differs":     mismatch(REV, unpOu),
			"F StartPayment, window open":              notPending(EXP, unpOu),
			"F StartPayment, window closed":            notPending(EXP, unpOu),
			"G' sweep finds the money, amount matches": result{status: REFING, outcome: unpOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"G'' sweep finds money, amount differs":    mismatch(REV, unpOu),
			"A' OrderConfirmed again without total":    a1(EXP, unpOu),
			"H refund succeeded":                       notRefunding(EXP, unpOu),
		}},
		{"CANCELLED by OrderCancelled", CAN, cancOu, true, cell{
			"B OrderCancelled":                         same(CAN, cancOu),
			"C OrderCompleted":                         same(CAN, cancOu),
			"D webhook capture, amount matches":        result{status: REFING, outcome: cancOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"D' webhook capture, amount differs":       mismatch(REV, cancOu),
			"D' webhook capture, currency differs":     mismatch(REV, cancOu),
			"F StartPayment, window open":              notPending(CAN, cancOu),
			"F StartPayment, window closed":            notPending(CAN, cancOu),
			"G' sweep finds the money, amount matches": result{status: REFING, outcome: cancOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"G'' sweep finds money, amount differs":    mismatch(REV, cancOu),
			"A' OrderConfirmed again without total":    a1(CAN, cancOu),
			"H refund succeeded":                       notRefunding(CAN, cancOu),
		}},
		{"CANCELLED by OrderCompleted", CAN, unpOu, true, cell{
			"B OrderCancelled":                         same(CAN, unpOu),
			"C OrderCompleted":                         same(CAN, unpOu),
			"D webhook capture, amount matches":        result{status: REFING, outcome: unpOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"D' webhook capture, amount differs":       mismatch(REV, unpOu),
			"D' webhook capture, currency differs":     mismatch(REV, unpOu),
			"F StartPayment, window open":              notPending(CAN, unpOu),
			"F StartPayment, window closed":            notPending(CAN, unpOu),
			"G' sweep finds the money, amount matches": result{status: REFING, outcome: unpOu, publish: pub(payment.EventCaptured, payment.EventRefundRequested)},
			"G'' sweep finds money, amount differs":    mismatch(REV, unpOu),
			"A' OrderConfirmed again without total":    a1(CAN, unpOu),
			"H refund succeeded":                       notRefunding(CAN, unpOu),
		}},
		{"REFUNDING", REFING, cancOu, true, cell{
			"D' webhook capture, amount differs":    mismatch(REFING, cancOu),
			"D' webhook capture, currency differs":  mismatch(REFING, cancOu),
			"F StartPayment, window open":           notPending(REFING, cancOu),
			"F StartPayment, window closed":         notPending(REFING, cancOu),
			"A' OrderConfirmed again without total": a1(REFING, cancOu),
			"H refund succeeded":                    result{status: REFED, outcome: cancOu, publish: pub(payment.EventRefunded)},
		}},
		{"REFUNDED", REFED, cancOu, true, cell{
			"D' webhook capture, amount differs":    mismatch(REFED, cancOu),
			"D' webhook capture, currency differs":  mismatch(REFED, cancOu),
			"F StartPayment, window open":           notPending(REFED, cancOu),
			"F StartPayment, window closed":         notPending(REFED, cancOu),
			"A' OrderConfirmed again without total": a1(REFED, cancOu),
			// the refund call is idempotent on the payment id: asking again refunds nothing twice
			"H refund succeeded": same(REFED, cancOu),
		}},
		{"NEEDS_REVIEW", REV, none, true, cell{
			"B OrderCancelled":                      result{status: REV, outcome: cancOu},
			"C OrderCompleted":                      result{status: REV, outcome: unpOu},
			"D webhook capture, amount matches":     result{status: REV, outcome: none, alert: payment.AlertCaptureAfterReview},
			"D' webhook capture, amount differs":    result{status: REV, outcome: none, alert: payment.AlertCaptureAfterReview},
			"D' webhook capture, currency differs":  result{status: REV, outcome: none, alert: payment.AlertCaptureAfterReview},
			"F StartPayment, window open":           notPending(REV, none),
			"F StartPayment, window closed":         notPending(REV, none),
			"A' OrderConfirmed again without total": a1(REV, none),
			"H refund succeeded":                    notRefunding(REV, none),
		}},
	}

	defaults := func(name string, st payment.Status, out payment.Outcome) result {
		switch name {
		case "F StartPayment, window open":
			if st == P {
				return result{status: st, outcome: out}
			}
		case "F StartPayment, window closed":
			if st == P {
				return result{status: st, outcome: out, err: payment.ErrExpired}
			}
		}

		return same(st, out)
	}

	for _, row := range rows {
		for _, ev := range events {
			t.Run(row.name+" × "+ev.name, func(t *testing.T) {
				x := fixture(t, row.st, row.out, row.checkout)
				want, ok := row.want[ev.name]
				if !ok {
					want = defaults(ev.name, row.st, row.out)
				}

				eff, err := ev.do(t, x)

				if !errors.Is(err, want.err) || (want.err == nil && err != nil) {
					t.Fatalf("err = %v, want %v", err, want.err)
				}
				if x.Status() != want.status {
					t.Errorf("status = %s, want %s", x.Status(), want.status)
				}
				if x.Outcome() != want.outcome {
					t.Errorf("order outcome = %s, want %s", x.Outcome(), want.outcome)
				}
				if !reflect.DeepEqual(eff.Publish(), want.publish) && (len(eff.Publish()) != 0 || len(want.publish) != 0) {
					t.Errorf("publish = %v, want %v", eff.Publish(), want.publish)
				}
				if eff.Alert() != want.alert {
					t.Errorf("alert = %q, want %q", eff.Alert(), want.alert)
				}
				if eff.CancelCheckout() != want.cancel {
					t.Errorf("cancel checkout = %v, want %v", eff.CancelCheckout(), want.cancel)
				}
			})
		}
	}
}

// Entering EXPIRED or CANCELLED stamps terminal_at: the reconcile window runs from it.
func TestTerminalAtStampedOnEntry(t *testing.T) {
	x := fixture(t, P, none, true)
	x.Unpaid(late)
	if x.Status() != EXP || !x.TerminalAt().Equal(late) {
		t.Fatalf("expired: status %s terminal_at %v, want EXPIRED at %v", x.Status(), x.TerminalAt(), late)
	}

	y := fixture(t, P, none, true)
	y.OrderCancelled(t0.Add(time.Minute))
	if y.Status() != CAN || !y.TerminalAt().Equal(t0.Add(time.Minute)) {
		t.Fatalf("cancelled: status %s terminal_at %v", y.Status(), y.TerminalAt())
	}
	if !y.UpdatedAt().Equal(t0.Add(time.Minute)) {
		t.Fatalf("updated_at = %v, want the transition time", y.UpdatedAt())
	}
}

func newParams(t *testing.T) payment.NewParams {
	t.Helper()

	return payment.NewParams{ID: "pay-1", OrderID: "ord-1", CustomerID: "cust-1", Total: money(t, 1000, "TWD"), ExpiresAt: expires}
}

// T1: OrderConfirmed creates a PENDING payment that has asked the provider for nothing.
func TestNew(t *testing.T) {
	x, err := payment.New(newParams(t), t0)
	if err != nil {
		t.Fatal(err)
	}
	if x.ID().String() != "pay-1" || x.OrderID() != "ord-1" || x.CustomerID() != "cust-1" ||
		x.Amount() != money(t, 1000, "TWD") || x.Status() != P || x.Outcome() != none ||
		!x.ExpiresAt().Equal(expires) || x.Version() != 0 || !x.UpdatedAt().Equal(t0) ||
		x.Provider() != "" || x.ProviderRef() != "" || x.CheckoutURL() != "" || !x.CheckoutExpiresAt().IsZero() || !x.TerminalAt().IsZero() {
		t.Fatalf("unexpected new payment: %+v", x)
	}
}

func TestNew_Refusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*payment.NewParams)
		want   error
	}{
		"no id":       {func(p *payment.NewParams) { p.ID = "" }, payment.ErrInvalidID},
		"no order":    {func(p *payment.NewParams) { p.OrderID = "" }, payment.ErrInvalidOrderID},
		"no customer": {func(p *payment.NewParams) { p.CustomerID = "" }, payment.ErrEmptyCustomer},
		// A missing total (the zero Money) is never read as a payment of zero (docs/payment.md §2).
		"no total":        {func(p *payment.NewParams) { p.Total = kernel.Money{} }, payment.ErrMissingTotal},
		"negative total":  {func(p *payment.NewParams) { p.Total = money(t, 5, "TWD").Multiply(-1) }, payment.ErrInvalidAmount},
		"window not open": {func(p *payment.NewParams) { p.ExpiresAt = t0 }, payment.ErrInvalidExpiry},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := newParams(t)
			c.mutate(&p)
			if x, err := payment.New(p, t0); !errors.Is(err, c.want) || x != nil {
				t.Fatalf("New = %v, %v; want %v", x, err, c.want)
			}
		})
	}
}

// T14: the order's end arrived before OrderConfirmed.
func TestNewForEndedOrder(t *testing.T) {
	for _, out := range []payment.Outcome{cancOu, unpOu} {
		x, err := payment.NewForEndedOrder(newParams(t), out, t0)
		if err != nil {
			t.Fatal(err)
		}
		if x.Status() != CAN || x.Outcome() != out || !x.TerminalAt().Equal(t0) {
			t.Fatalf("outcome %s: status %s outcome %s terminal_at %v", out, x.Status(), x.Outcome(), x.TerminalAt())
		}
		// no one can pay it
		if err := x.CheckStart(t0); !errors.Is(err, payment.ErrNotPending) {
			t.Fatalf("CheckStart = %v, want not pending", err)
		}
	}
	if _, err := payment.NewForEndedOrder(newParams(t), none, t0); !errors.Is(err, payment.ErrInvalidOutcome) {
		t.Fatalf("outcome NONE: err = %v", err)
	}
	p := newParams(t)
	p.Total = kernel.Money{}
	if _, err := payment.NewForEndedOrder(p, cancOu, t0); !errors.Is(err, payment.ErrMissingTotal) {
		t.Fatalf("no total: err = %v", err)
	}
}

// A row nobody wrote through the aggregate must not load into a state no transition can produce.
func TestReconstitute_Refusals(t *testing.T) {
	base := payment.ReconstituteParams{
		ID: "pay-1", OrderID: "ord-1", CustomerID: "cust-1", Amount: money(t, 1000, "TWD"),
		Status: P, ExpiresAt: expires, Outcome: none,
	}
	cases := map[string]struct {
		mutate func(*payment.ReconstituteParams)
		want   error
	}{
		"no id":                   {func(p *payment.ReconstituteParams) { p.ID = "" }, payment.ErrInvalidID},
		"no order":                {func(p *payment.ReconstituteParams) { p.OrderID = "" }, payment.ErrInvalidOrderID},
		"no customer":             {func(p *payment.ReconstituteParams) { p.CustomerID = "" }, payment.ErrEmptyCustomer},
		"unknown status":          {func(p *payment.ReconstituteParams) { p.Status = "PAID" }, payment.ErrInvalidStatus},
		"unknown outcome":         {func(p *payment.ReconstituteParams) { p.Outcome = "GONE" }, payment.ErrInvalidOutcome},
		"pending that heard end":  {func(p *payment.ReconstituteParams) { p.Outcome = cancOu }, payment.ErrInvalidOutcome},
		"captured that heard end": {func(p *payment.ReconstituteParams) { p.Status, p.Outcome = CAP, unpOu }, payment.ErrInvalidOutcome},
		"cancelled without end":   {func(p *payment.ReconstituteParams) { p.Status = CAN }, payment.ErrInvalidOutcome},
		"refunding without end":   {func(p *payment.ReconstituteParams) { p.Status = REFING }, payment.ErrInvalidOutcome},
		"refunded without end":    {func(p *payment.ReconstituteParams) { p.Status = REFED }, payment.ErrInvalidOutcome},
		"no currency":             {func(p *payment.ReconstituteParams) { p.Amount = kernel.Money{} }, payment.ErrInvalidAmount},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			c.mutate(&p)
			if x, err := payment.Reconstitute(p); !errors.Is(err, c.want) || x != nil {
				t.Fatalf("Reconstitute = %v, %v; want %v", x, err, c.want)
			}
		})
	}
	if _, err := payment.Reconstitute(base); err != nil {
		t.Fatalf("the base params must load: %v", err)
	}
}

func TestStartPaymentCheckout(t *testing.T) {
	x := fixture(t, P, none, false)
	if url, ok := x.ReusableCheckout(t0); ok || url != "" {
		t.Fatalf("a payment that never opened a checkout has none to reuse: %q %v", url, ok)
	}

	checkoutEnds := t0.Add(10 * time.Minute)
	if err := x.AttachCheckout("mock", "ref-1", "https://pay.example/c/1", checkoutEnds, t0); err != nil {
		t.Fatal(err)
	}
	if x.Provider() != "mock" || x.ProviderRef() != "ref-1" || x.CheckoutURL() != "https://pay.example/c/1" ||
		!x.CheckoutExpiresAt().Equal(checkoutEnds) || x.Status() != P {
		t.Fatalf("checkout not recorded: %+v", x)
	}
	if url, ok := x.ReusableCheckout(checkoutEnds.Add(-time.Second)); !ok || url != "https://pay.example/c/1" {
		t.Fatalf("reusable while valid = %q %v", url, ok)
	}
	// the page stops working at checkout_expires_at: from then on a new one is needed
	if url, ok := x.ReusableCheckout(checkoutEnds); ok || url != "" {
		t.Fatalf("reusable at its expiry = %q %v, want none", url, ok)
	}
}

func TestAttachCheckout_Refusals(t *testing.T) {
	ends := t0.Add(10 * time.Minute)
	cases := map[string]struct {
		x    *payment.Payment
		now  time.Time
		args [4]string
		end  time.Time
		want error
	}{
		"not pending":   {fixture(t, CAP, none, true), t0, [4]string{"mock", "r", "u"}, ends, payment.ErrNotPending},
		"window closed": {fixture(t, P, none, false), late, [4]string{"mock", "r", "u"}, late.Add(time.Minute), payment.ErrExpired},
		"no provider":   {fixture(t, P, none, false), t0, [4]string{"", "r", "u"}, ends, payment.ErrInvalidCheckout},
		"no ref":        {fixture(t, P, none, false), t0, [4]string{"mock", "", "u"}, ends, payment.ErrInvalidCheckout},
		"no url":        {fixture(t, P, none, false), t0, [4]string{"mock", "r", ""}, ends, payment.ErrInvalidCheckout},
		"already ended": {fixture(t, P, none, false), t0, [4]string{"mock", "r", "u"}, t0, payment.ErrInvalidCheckout},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.x.AttachCheckout(c.args[0], c.args[1], c.args[2], c.end, c.now); !errors.Is(err, c.want) {
				t.Fatalf("AttachCheckout = %v, want %v", err, c.want)
			}
		})
	}
}

func TestStatusesAndStrings(t *testing.T) {
	want := []string{"PENDING", "CAPTURED", "EXPIRED", "CANCELLED", "REFUNDING", "REFUNDED", "NEEDS_REVIEW"}
	got := payment.Statuses()
	if len(got) != len(want) {
		t.Fatalf("Statuses = %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("Statuses[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if cancOu.String() != "CANCELLED" {
		t.Fatal("outcome string")
	}
}

// ErrStale is a lost race: retry, never a final domain answer.
func TestErrStaleIsAConcurrentUpdate(t *testing.T) {
	if !errors.Is(payment.ErrStale, kernel.ErrConcurrentUpdate) {
		t.Fatal("ErrStale must wrap kernel.ErrConcurrentUpdate")
	}
	var de *kernel.Error
	if errors.As(payment.ErrStale, &de) {
		t.Fatal("ErrStale must not be a kernel.Error: a consumer would drop the message instead of retrying")
	}
}

func TestNewEffect(t *testing.T) {
	in := []payment.Event{payment.EventCaptured, payment.EventRefundRequested}
	e := payment.NewEffect(in, payment.AlertAmountMismatch, true)
	in[0] = payment.EventExpired // the caller's slice is not shared
	got := e.Publish()
	if len(got) != 2 || got[0] != payment.EventCaptured || e.Alert() != payment.AlertAmountMismatch || !e.CancelCheckout() {
		t.Fatalf("effect = %v %q %v", got, e.Alert(), e.CancelCheckout())
	}
	got[1] = payment.EventExpired // nor is the one it hands out
	if e.Publish()[1] != payment.EventRefundRequested {
		t.Fatal("Publish hands out the effect's own slice")
	}
	if z := (payment.Effect{}); len(z.Publish()) != 0 || z.Alert() != payment.AlertNone || z.CancelCheckout() {
		t.Fatal("the zero Effect must be 'nothing'")
	}
}
