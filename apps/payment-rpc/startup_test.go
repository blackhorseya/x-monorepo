package main

import (
	"strings"
	"testing"
)

// The command line: serve and expire are the subcommands (docs/payment.md §1). Anything else is refused before anything is opened, and the refusal
// does not echo the argument.
func TestRun_Subcommand(t *testing.T) {
	for name, args := range map[string][]string{
		"none":                        nil,
		"a flag first":                {"--require-durable"},
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
					env["APP_PAYMENT_WINDOW"] = s("10m") // inside every page below: only the reconcile window is judged here
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

// docs/payment.md T2: one payment has one checkout for its whole window, so payment.window must fit in
// what the provider can keep valid (its checkout validity). Judged on the final effective settings, at
// start-up, with or without --require-durable.
func TestRun_PaymentWindowMustFitInOneCheckout(t *testing.T) {
	for _, flags := range [][]string{{"serve"}, {"serve", "--require-durable"}} {
		for name, c := range map[string]struct {
			window, ttl string
			refused     bool
		}{
			"defaults agree":               {"", "", false},
			"window equal to the pages":    {"30m", "30m", false},
			"window one second too long":   {"30m1s", "30m", true},
			"window longer than the pages": {"1h", "15m", true},
			"short window, long pages":     {"5m", "30m", false},
		} {
			t.Run(strings.Join(flags, " ")+": "+name, func(t *testing.T) {
				env := durableEnv(t, nil)
				if c.window != "" {
					env["APP_PAYMENT_WINDOW"] = s(c.window)
				}
				if c.ttl != "" {
					env["APP_PAYMENT_MOCK_CHECKOUT_TTL"] = s(c.ttl)
				}
				out := start(t, flags, env)
				if c.refused {
					if out.served || out.err == nil || !strings.Contains(out.err.Error(), "payment.window") {
						t.Fatalf("run = %+v, want a refusal naming payment.window", out)
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

// The settings the saga added fail closed at start-up too, for every command, judged on the final values:
// the webhook log is the dedup memory of redelivered notifications, so it may not be shorter than the
// provider's redelivery window (the mock says 72h); 0 keeps everything; the rest must be positive.
func TestRun_SagaSettingsFailClosed(t *testing.T) {
	newConsumeEnv(t) // migrates ordering: the saga harness puts its cursor on ordering's event tables
	for _, cmd := range [][]string{{"serve"}, {"expire"}} {
		for name, c := range map[string]struct {
			env, value string
			refused    bool
			names      string
		}{
			"retention shorter than the redelivery window": {"APP_PAYMENT_WEBHOOK_RETENTION", "10h", true, "payment.webhook_retention"},
			"retention equal to the redelivery window":     {"APP_PAYMENT_WEBHOOK_RETENTION", "72h", true, "payment.webhook_retention"},
			"retention a second longer":                    {"APP_PAYMENT_WEBHOOK_RETENTION", "72h1s", false, ""},
			"retention 0 keeps everything":                 {"APP_PAYMENT_WEBHOOK_RETENTION", "0s", false, ""},
			"negative retention":                           {"APP_PAYMENT_WEBHOOK_RETENTION", "-1h", true, "payment.webhook_retention"},
			"no payment window":                            {"APP_PAYMENT_WINDOW", "0s", true, "payment.window"},
			"no expire interval":                           {"APP_PAYMENT_EXPIRE_INTERVAL", "0s", true, "payment.expire_interval"},
			"no expire batch":                              {"APP_PAYMENT_EXPIRE_BATCH", "0", true, "payment.expire_batch"},
		} {
			t.Run(cmd[0]+": "+name, func(t *testing.T) {
				env := durableEnv(t, map[string]*string{c.env: s(c.value)})
				if cmd[0] != "serve" {
					out := startAndStop(t, cmd, env)
					if c.refused {
						if out.err == nil || !strings.Contains(out.err.Error(), c.names) {
							t.Fatalf("run = %+v, want a refusal naming %s", out, c.names)
						}
					} else if out.err != nil {
						t.Fatalf("run = %+v, want a clean run", out)
					}

					return
				}
				out := start(t, cmd, env)
				if c.refused {
					if out.served || out.err == nil || !strings.Contains(out.err.Error(), c.names) {
						t.Fatalf("run = %+v, want a refusal naming %s", out, c.names)
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
