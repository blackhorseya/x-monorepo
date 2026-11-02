package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const payModule = "github.com/blackhorseya/x-monorepo/pkgs/payment"

// importsOf lists the import paths of the non-test Go files of dir.
func importsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			out[p] = append(out[p], filepath.Base(f))
		}
	}

	return out
}

// apps/rpc does not load payment (docs/payment.md §1, the way #320 did for the catalog): of the payment
// BC it imports its contract and its client, nothing of its domain, application, infra or rpc adapter, and
// the client itself reaches no further into the BC than the contract. Positive controls: both really are
// imported (otherwise the rule would pass on an rpc that has no payment at all).
func TestRPC_PaymentLivesBehindTheNetwork(t *testing.T) {
	allowed := map[string]bool{
		payModule + "/contract/v1":                  true,
		payModule + "/contract/v1/paymentv1connect": true,
		payModule + "/adapters/client":              true,
	}
	rpc := importsOf(t, ".")
	for p, files := range rpc {
		if (p == payModule || strings.HasPrefix(p, payModule+"/")) && !allowed[p] {
			t.Errorf("apps/rpc imports %s (%v): it may import only payment's contract and adapters/client", p, files)
		}
	}
	// The client is what rpc imports (its wire set and the service). The contract comes with it, not from rpc.
	if rpc[payModule+"/adapters/client"] == nil {
		t.Errorf("apps/rpc does not import %s (positive control)", payModule+"/adapters/client")
	}

	dir := filepath.Join("..", "..", "pkgs", "payment", "adapters", "client")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("payment's client package: %v", err)
	}
	for p, files := range importsOf(t, dir) {
		if (p == payModule || strings.HasPrefix(p, payModule+"/")) && !strings.HasPrefix(p, payModule+"/contract/") {
			t.Errorf("payment's client imports %s (%v): it may import only payment's contract", p, files)
		}
	}
}
