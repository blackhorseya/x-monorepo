package main

import (
	"strings"
	"testing"
)

// The command line: serve and consume are the subcommands so far (docs/payment.md §1 names expire too). Anything else is refused before anything is opened, and the refusal
// does not echo the argument.
func TestRun_Subcommand(t *testing.T) {
	for name, args := range map[string][]string{
		"none":                        nil,
		"a flag first":                {"--require-durable"},
		"expire does not exist yet":   {"expire"},
		"a typo":                      {"sreve"},
		"serve with an unknown flag":  {"serve", "--verbose"},
		"serve with a stray argument": {"serve", "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			out := start(t, args, durableEnv(t, nil))
			if out.served || out.err == nil {
				t.Fatalf("run(%q) = %+v, want a refusal", args, out)
			}
			for _, a := range args {
				if a != "serve" && a != "--require-durable" && strings.Contains(out.err.Error(), a) {
					t.Fatalf("the refusal echoes the argument %q: %v", a, out.err)
				}
			}
		})
	}
}

// docs/payment.md §3「補查有界」: payment.reconcile_window must be longer than the provider's checkout
// validity, or a customer who pays at the very end of a page could pay after the sweep stopped looking.
// Judged on the final effective settings, at start-up, with or without --require-durable.
func TestRun_ReconcileWindowMustOutlastCheckoutPages(t *testing.T) {
	for _, flags := range [][]string{{"serve"}, {"serve", "--require-durable"}} {
		for name, c := range map[string]struct {
			window, ttl string
			refused     bool
		}{
			"default window, default pages": {"", "", false},
			"window shorter than the pages": {"10m", "15m", true},
			"window equal to the pages":     {"15m", "15m", true},
			"window one second longer":      {"15m1s", "15m", false},
			"no window":                     {"0s", "15m", true},
			"negative window":               {"-1h", "15m", true},
			"long pages need a long window": {"24h", "25h", true},
		} {
			t.Run(strings.Join(flags, " ")+": "+name, func(t *testing.T) {
				env := durableEnv(t, nil)
				if c.window != "" {
					env["APP_PAYMENT_RECONCILE_WINDOW"] = s(c.window)
				}
				if c.ttl != "" {
					env["APP_PAYMENT_MOCK_CHECKOUT_TTL"] = s(c.ttl)
				}
				out := start(t, flags, env)
				if c.refused {
					if out.served || out.err == nil || !strings.Contains(out.err.Error(), "payment.reconcile_window") {
						t.Fatalf("run = %+v, want a refusal naming payment.reconcile_window", out)
					}

					return
				}
				if !out.served || out.err != nil {
					t.Fatalf("run = %+v, want a clean start and stop", out)
				}
			})
		}
	}
}

// Only the mock exists; any other provider name fails closed instead of falling back to it.
func TestRun_UnknownProviderIsRefused(t *testing.T) {
	out := start(t, []string{"serve"}, durableEnv(t, map[string]*string{"APP_PAYMENT_PROVIDER": s("stripe")}))
	if out.served || out.err == nil || !strings.Contains(out.err.Error(), "payment.provider") {
		t.Fatalf("run = %+v, want a refusal naming payment.provider", out)
	}
}
