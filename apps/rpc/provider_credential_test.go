package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rpc is told where payment-rpc is and the service key to present (payment.upstream / payment.api_key); it never needs
// the provider's credential, and it reads the release-wide ConfigMap and Secret an operator can write. An rpc whose
// FINAL loaded config carries payment.mock_webhook_secret refuses to start, by whichever way it got there.
func TestRun_RefusesAProviderCredential(t *testing.T) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	t.Setenv("APP_HTTP_ADDR", freeAddr(t))
	cat := startCatalogProc(t) // rpc refuses to start without a catalog upstream
	t.Setenv("APP_CATALOG_UPSTREAM", cat.URL)
	t.Setenv("APP_CATALOG_API_KEY", serviceKey)
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_HTTP_SHUTDOWN_DELAY", "0")
	const credential = "mock-webhook-secret-0123456789"
	t.Setenv("APP_PAYMENT_MOCK_WEBHOOK_SECRET", credential)

	done := make(chan error, 1)
	go func() { done <- run() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "payment.mock_webhook_secret") {
				t.Fatalf("run = %v, want a refusal naming payment.mock_webhook_secret", err)
			}
			if strings.Contains(err.Error(), credential) {
				t.Fatalf("the refusal prints the credential: %v", err)
			}
			return
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			// it is serving: stop it (repeatedly: a signal sent before NotifyContext is installed is lost), then fail
			for range 20 {
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				select {
				case <-done:
					t.Fatal("rpc started with a provider credential in its config")
				case <-time.After(300 * time.Millisecond):
				}
			}
			t.Fatal("rpc started with a provider credential in its config and did not stop")
		}
	}
}
