package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"google.golang.org/protobuf/types/known/durationpb"

	inventorypg "github.com/blackhorseya/x-monorepo/pkgs/inventory/infra/postgres"
	orderingpg "github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	paymentpg "github.com/blackhorseya/x-monorepo/pkgs/payment/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

// docs/payment.md §9 (slice 3, poison / prune): the admin RPC serves payment's poison queue next to
// ordering's and inventory's, naming the schema by the contract's constant. A message parked by
// `payment-worker` after its retries is listed — by BC and across every BC — and the BC is a
// known one (not "unknown bounded context").
func TestAdminPoison_ServesPayment(t *testing.T) {
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := config.Postgres{URL: pgtest.URL(t)}
	tel := pgtest.Telemetry(t)
	db := pgtest.DB(t, cfg, tel)
	for _, m := range []postgres.Migration{orderingpg.Migration(), inventorypg.Migration(), paymentpg.Migration()} {
		if err := postgres.Up(c, cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.WithContext(c).Exec("TRUNCATE payment.events_poison").Error; err != nil {
		t.Fatal(err)
	}
	pub, err := eventbus.NewPostgresPublisher(db, tel.Logger)
	if err != nil {
		t.Fatal(err)
	}
	msg := message.NewMessage(watermill.NewUUID(), []byte("refund me"))
	msg.Metadata.Set(middleware.PoisonedTopicKey, "payment.v1.PaymentRefundRequested")
	msg.Metadata.Set(middleware.PoisonedHandlerKey, "payment.v1.PaymentRefundRequested")
	msg.Metadata.Set(middleware.ReasonForPoisonedKey, "provider down")
	if err := pub.Publish(eventbus.PoisonTopic(paymentv1.Schema), msg); err != nil {
		t.Fatal(err)
	}

	svc := NewPoisonService(db, config.Auth{})
	for name, bc := range map[string]string{"by BC": paymentv1.Schema, "every BC": ""} {
		res, err := svc.ListPoison(c, connect.NewRequest(&adminv1.ListPoisonRequest{BoundedContext: bc}))
		if err != nil {
			t.Fatalf("%s: ListPoison: %v", name, err)
		}
		var found bool
		for _, m := range res.Msg.GetMessages() {
			found = found || (m.GetBoundedContext() == paymentv1.Schema && m.GetUuid() == msg.UUID)
		}
		if !found {
			t.Errorf("%s: payment's parked message is not in %v", name, res.Msg.GetMessages())
		}
	}
	pruned, err := svc.Prune(c, connect.NewRequest(&adminv1.PruneRequest{BoundedContext: paymentv1.Schema, OlderThan: durationpb.New(24 * time.Hour), DryRun: true}))
	if err != nil {
		t.Fatalf("dry-run prune of payment: %v", err)
	}
	var inbox bool
	for _, tbl := range pruned.Msg.GetTables() {
		inbox = inbox || tbl.GetTable() == "payment.inbox"
	}
	if !inbox {
		t.Errorf("prune of payment did not reach payment.inbox: %v", pruned.Msg.GetTables())
	}
}

// docs/payment.md §1: apps/rpc links payment's contract and its client and nothing else of the BC — the
// transitive import graph of the binary, not just its own imports (a package that is imported by an
// allowed one is still linked). Positive control: the client is in the graph.
func TestRPC_LinksPaymentContractAndClientOnly(t *testing.T) {
	const mod = "github.com/blackhorseya/x-monorepo/pkgs/payment"
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	var client bool
	for _, p := range strings.Fields(string(out)) {
		switch {
		case p == mod+"/adapters/client":
			client = true
		case p == mod+"/contract/v1", p == mod+"/contract/v1/paymentv1connect":
		case p == mod || strings.HasPrefix(p, mod+"/"):
			t.Errorf("apps/rpc links %s: only payment's contract and adapters/client may be linked", p)
		}
	}
	if !client {
		t.Errorf("apps/rpc does not link %s/adapters/client (positive control)", mod)
	}
}
