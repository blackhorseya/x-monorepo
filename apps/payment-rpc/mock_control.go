package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/webhook"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// The subcommands of `payment-rpc mock` (docs/payment.md §9 S6).
const (
	mockPay    = "pay"
	mockNotify = "notify"
	mockFail   = "fail"
)

// mockTimeout bounds the whole command, including a delivery to the webhook route. A variable only so the
// regression test can make the wait short; nothing else sets it.
var mockTimeout = 30 * time.Second

// printedDelivery is what `mock pay` and `mock notify` print: one JSON object with the signed webhook to
// deliver (body is the raw JSON text; the signature is over exactly those bytes). Status is set when
// --deliver posted it.
type printedDelivery struct {
	Ref       string `json:"ref"`
	EventID   string `json:"event_id"`
	Body      string `json:"body"`
	Signature string `json:"signature"`
	Status    int    `json:"delivered_status,omitempty"`
}

// mockFlags holds the flags of one subcommand.
type mockFlags struct {
	order, ref, id, typ, deliver, currency, ops string
	amount                                      int64
	amountSet                                   bool
}

func newMockFlagSet(name string, f *mockFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("payment-rpc mock "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch name {
	case mockPay:
		fs.StringVar(&f.order, "order", "", "the order whose checkout the customer pays")
		fs.StringVar(&f.deliver, "deliver", "", "POST the signed webhook to this URL (the serve process's /api/payment/webhook)")
		fs.Int64Var(&f.amount, "amount", 0, "pay this amount instead of the payment's (minor units; needs --currency)")
		fs.StringVar(&f.currency, "currency", "", "the currency of --amount")
	case mockNotify:
		fs.StringVar(&f.ref, "ref", "", "the checkout the notification is about")
		fs.StringVar(&f.id, "id", "", "the provider's event id (the same id again is a redelivery)")
		fs.StringVar(&f.typ, "type", "", "the event type, e.g. payment.captured")
		fs.StringVar(&f.deliver, "deliver", "", "POST the signed webhook to this URL")
		fs.Int64Var(&f.amount, "amount", 0, "the amount in the event (default: the checkout's)")
		fs.StringVar(&f.currency, "currency", "", "the currency of --amount")
	case mockFail:
		fs.StringVar(&f.ops, "op", "", "comma-separated provider operations (create_checkout, get_payment, cancel_checkout, refund); default all")
	}

	return fs
}

// runMock is `payment-rpc mock pay|notify|fail`: it parses first (nothing is read before the arguments are
// sound), then checks that the entry may exist (provider mock, payment.mock_control, a secret, a database), then
// works on the mock tables. A refusal prints nothing and changes nothing.
func runMock(args []string, out io.Writer) error {
	const usage = "payment-rpc mock: a subcommand is required: pay, notify or fail"
	if len(args) == 0 {
		return errors.New(usage)
	}
	sub, rest := args[0], args[1:]
	if sub != mockPay && sub != mockNotify && sub != mockFail {
		return fmt.Errorf("payment-rpc mock: unknown subcommand %q (want pay, notify or fail)", sub)
	}

	on := false
	if sub == mockFail {
		if len(rest) == 0 || (rest[0] != "on" && rest[0] != "off") {
			return errors.New("payment-rpc mock fail: say `on` or `off` (then --op to name operations)")
		}
		on, rest = rest[0] == "on", rest[1:]
	}
	var f mockFlags
	fs := newMockFlagSet(sub, &f)
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("payment-rpc mock %s: %w", sub, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("payment-rpc mock %s: unexpected argument %q", sub, fs.Arg(0))
	}
	fs.Visit(func(fl *flag.Flag) { f.amountSet = f.amountSet || fl.Name == "amount" })
	var amount *kernel.Money
	if f.amountSet {
		m, err := kernel.NewMoney(f.amount, f.currency)
		if err != nil {
			return fmt.Errorf("payment-rpc mock %s: --amount %d --currency %q: %w", sub, f.amount, f.currency, err)
		}
		amount = &m
	}
	if sub == mockPay && strings.TrimSpace(f.order) == "" {
		return errors.New("payment-rpc mock pay: --order is required: the order whose checkout is paid")
	}

	// The whole command is bounded by mockTimeout (#406): the database is opened, read and written under this context,
	// and if something no context reaches (opening the pool) holds past it, the deadline still ends the command. The
	// body is left to finish in the background; every write it can still make is under c, which is cancelled by then.
	c, cancel := context.WithTimeout(context.Background(), mockTimeout)
	defer cancel()
	done := make(chan error, 1)
	// fire-and-forget: the command ends at the deadline below; the goroutine's only effects are under c, which is cancelled then
	go func() { done <- runMockBody(c, sub, f, on, amount, out) }()
	select {
	case err := <-done:
		// the server's own timeout (below) can end the statement a hair before the context does
		if err != nil && strings.Contains(err.Error(), "statement timeout") {
			return fmt.Errorf("payment-rpc mock %s: deadline exceeded: %w", sub, err)
		}

		return err
	case <-c.Done():
		return fmt.Errorf("payment-rpc mock %s: %w", sub, c.Err())
	}
}

// runMockBody is the command after its arguments are sound, under c.
func runMockBody(c context.Context, sub string, f mockFlags, on bool, amount *kernel.Money, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Postgres.URL) != "" {
		// Cancelling the context abandons the connection, and a backend that is waiting on a lock does not notice: it
		// would make the write when the lock is let go, after the command had already failed. The server enforces the
		// deadline itself, lock waits included, and it does so BEFORE the context's deadline (three quarters of what is
		// left): the statement is over on the server when the command reports. (A statement that only starts in the last
		// quarter of the budget is not covered; opening the pool and the lookups come first and are fast.)
		if dl, ok := c.Deadline(); ok {
			cfg.Postgres.URL = withStatementTimeout(cfg.Postgres.URL, time.Until(dl)*3/4)
		}
	}
	if strings.TrimSpace(cfg.Postgres.URL) == "" {
		return errors.New("payment-rpc mock: postgres.url is required: the control works on the mock tables the payment-rpc processes share")
	}
	tel, telCleanup, err := otel.New(cfg.OTel, cfg.Log)
	if err != nil {
		return err
	}
	defer telCleanup()
	db, cleanup, err := postgres.NewDB(cfg.Postgres, tel)
	if err != nil {
		return err
	}
	defer cleanup()
	ctl, err := payment.NewMockControl(cfg.Payment, db)
	if err != nil {
		return fmt.Errorf("payment-rpc mock: %w", err)
	}

	switch sub {
	case mockFail:
		var ops []string
		if f.ops != "" {
			ops = strings.Split(f.ops, ",")
		}

		return ctl.Fail(c, ops, on)
	case mockPay:
		p, err := ctl.Pay(c, f.order, amount)
		if err != nil {
			return fmt.Errorf("payment-rpc mock pay: %w", err)
		}

		return emit(c, out, printedDelivery{Ref: p.Ref, EventID: p.Delivery.EventID, Body: string(p.Delivery.Body), Signature: p.Delivery.Signature}, f.deliver)
	default:
		p, err := ctl.Notify(c, f.ref, f.id, f.typ, amount)
		if err != nil {
			return fmt.Errorf("payment-rpc mock notify: %w", err)
		}

		return emit(c, out, printedDelivery{Ref: p.Ref, EventID: p.Delivery.EventID, Body: string(p.Delivery.Body), Signature: p.Delivery.Signature}, f.deliver)
	}
}

// emit prints the delivery, after posting it to url when one is given. A refused delivery (any non-2xx) is the
// command's failure and prints nothing: a script waiting for "delivered" must not read a 4xx as one.
func emit(c context.Context, out io.Writer, d printedDelivery, url string) error {
	if url != "" {
		req, err := http.NewRequestWithContext(c, http.MethodPost, url, bytes.NewReader([]byte(d.Body)))
		if err != nil {
			return fmt.Errorf("--deliver %q: %w", url, err)
		}
		req.Header.Set(webhook.SignatureHeader, d.Signature)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("deliver the webhook: %w", err)
		}
		_ = res.Body.Close()
		d.Status = res.StatusCode
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return fmt.Errorf("the webhook route refused the delivery: status %d", res.StatusCode)
		}
	}

	return json.NewEncoder(out).Encode(d)
}

// withStatementTimeout adds statement_timeout=d (milliseconds, at least 1) to a Postgres URL or key/value DSN, so
// the server aborts any statement of this connection that runs longer, lock waits included.
func withStatementTimeout(raw string, d time.Duration) string {
	ms := max(d.Milliseconds(), 1)
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("statement_timeout", fmt.Sprint(ms))
		u.RawQuery = q.Encode()

		return u.String()
	}

	return fmt.Sprintf("%s statement_timeout=%d", raw, ms)
}
