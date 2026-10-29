package usecase_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

const adminRole = "root" // not "admin": the role name is injected, never assumed

var (
	alice = contextx.Principal{Subject: "acct-alice", Roles: []string{"user"}, Method: "api_key"}
	bob   = contextx.Principal{Subject: "acct-bob", Method: "jwt"}
	ops   = contextx.Principal{Subject: "acct-ops", Roles: []string{adminRole}, Method: "api_key"}
)

func as(p contextx.Principal) context.Context {
	return contextx.WithPrincipal(context.Background(), p)
}

// repos runs fn against every order.Repository implementation, so ownership is
// proven to be a property of the use case and not of one store.
func repos(t *testing.T, fn func(t *testing.T, repo order.Repository)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, memory.NewOrderRepository()) })
	t.Run("postgres", func(t *testing.T) {
		url := pgtest.URL(t)
		cfg := config.Postgres{URL: url}
		tel, telCleanup, err := otel.New(config.OTel{}, config.Log{Level: "warn", Format: "text"})
		if err != nil {
			t.Fatal(err)
		}
		defer telCleanup()
		db, cleanup, err := platformpg.NewDB(cfg, tel)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if err := platformpg.Up(context.Background(), cfg, postgres.Migration()); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`TRUNCATE ordering.orders CASCADE`).Error; err != nil {
			t.Fatal(err)
		}
		fn(t, postgres.NewOrderRepository(db))
	})
}

type seqIDs struct{ n int }

func (i *seqIDs) NewID(context.Context) (string, error) {
	i.n++
	return "o-" + string(rune('a'+i.n-1)), nil
}

func newCreate(repo order.Repository) *usecase.CreateOrder {
	return usecase.NewCreateOrder(repo, &recorder{}, &seqIDs{}, fixedClock{t: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)}, ownership.New(adminRole))
}

var oneItem = []dto.LineItem{{SKU: "SKU-1", Quantity: 1, UnitPrice: dto.Money{AmountMinor: 500, Currency: "USD"}}}

func place(t *testing.T, create *usecase.CreateOrder, p contextx.Principal, customer string) *dto.OrderOutput {
	t.Helper()
	out, err := create.Execute(as(p), dto.CreateOrderInput{CustomerID: customer, Items: oneItem})
	if err != nil {
		t.Fatalf("place as %s for %q: %v", p.Subject, customer, err)
	}
	return out
}

func wantDenied(t *testing.T, err error) {
	t.Helper()
	var ke *kernel.Error
	if !errors.As(err, &ke) || ke.Kind() != kernel.KindPermissionDenied || ke.Code() != "order.permission_denied" {
		t.Fatalf("err = %v, want order.permission_denied (KindPermissionDenied)", err)
	}
}

func TestCreateOrder_Ownership(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		create := newCreate(repo)

		// A principal's orders are theirs: no customer_id needed, Subject is used.
		if got := place(t, create, alice, ""); got.CustomerID != alice.Subject {
			t.Fatalf("customer = %q, want %q", got.CustomerID, alice.Subject)
		}
		// Naming yourself is fine.
		if got := place(t, create, alice, alice.Subject); got.CustomerID != alice.Subject {
			t.Fatalf("customer = %q, want %q", got.CustomerID, alice.Subject)
		}
		// Naming somebody else is not, and nothing is stored.
		_, err := create.Execute(as(alice), dto.CreateOrderInput{CustomerID: bob.Subject, Items: oneItem})
		wantDenied(t, err)
		if page, err := usecase.NewListOrders(repo, ownership.New(adminRole)).Execute(as(ops), dto.ListOrdersInput{CustomerID: bob.Subject}); err != nil || len(page.Orders) != 0 {
			t.Fatalf("denied create left %d orders (err %v)", len(page.Orders), err)
		}
		// An admin orders on behalf of a customer, or for themselves when none is named.
		if got := place(t, create, ops, "walk-in-7"); got.CustomerID != "walk-in-7" {
			t.Fatalf("admin customer = %q, want walk-in-7", got.CustomerID)
		}
		if got := place(t, create, ops, ""); got.CustomerID != ops.Subject {
			t.Fatalf("admin default customer = %q, want %q", got.CustomerID, ops.Subject)
		}
		// Anonymous (auth off): the existing rule, customer_id is required.
		if got := place(t, create, contextx.Principal{}, "c-anon"); got.CustomerID != "c-anon" {
			t.Fatalf("anonymous customer = %q, want c-anon", got.CustomerID)
		}
		if _, err := create.Execute(context.Background(), dto.CreateOrderInput{Items: oneItem}); !errors.Is(err, order.ErrEmptyCustomer) {
			t.Fatalf("anonymous without customer err = %v, want ErrEmptyCustomer", err)
		}
	})
}

func TestListOrders_Ownership(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		create := newCreate(repo)
		place(t, create, alice, "")
		place(t, create, alice, "")
		place(t, create, bob, "")
		list := usecase.NewListOrders(repo, ownership.New(adminRole))

		// Without a filter a non-admin sees exactly their own.
		out, err := list.Execute(as(alice), dto.ListOrdersInput{})
		if err != nil || len(out.Orders) != 2 {
			t.Fatalf("alice lists %d (err %v), want 2", len(out.Orders), err)
		}
		for _, o := range out.Orders {
			if o.CustomerID != alice.Subject {
				t.Fatalf("alice sees %s's order", o.CustomerID)
			}
		}
		// Naming yourself is the same thing; naming somebody else is denied.
		if out, err = list.Execute(as(alice), dto.ListOrdersInput{CustomerID: alice.Subject}); err != nil || len(out.Orders) != 2 {
			t.Fatalf("alice by name lists %d (err %v), want 2", len(out.Orders), err)
		}
		_, err = list.Execute(as(alice), dto.ListOrdersInput{CustomerID: bob.Subject})
		wantDenied(t, err)

		// Paging through the implicit filter works: the token binds the resolved customer.
		first, err := list.Execute(as(alice), dto.ListOrdersInput{PageSize: 1})
		if err != nil || len(first.Orders) != 1 || first.NextPageToken == "" {
			t.Fatalf("first page = %+v, %v", first, err)
		}
		second, err := list.Execute(as(alice), dto.ListOrdersInput{PageSize: 1, PageToken: first.NextPageToken})
		if err != nil || len(second.Orders) != 1 || second.Orders[0].ID == first.Orders[0].ID {
			t.Fatalf("second page = %+v, %v", second, err)
		}

		// Admin and anonymous keep today's behaviour: everything, or whoever they name.
		for name, ctx := range map[string]context.Context{"admin": as(ops), "anonymous": context.Background()} {
			if out, err = list.Execute(ctx, dto.ListOrdersInput{}); err != nil || len(out.Orders) != 3 {
				t.Fatalf("%s lists %d (err %v), want 3", name, len(out.Orders), err)
			}
			if out, err = list.Execute(ctx, dto.ListOrdersInput{CustomerID: bob.Subject}); err != nil || len(out.Orders) != 1 {
				t.Fatalf("%s lists bob's %d (err %v), want 1", name, len(out.Orders), err)
			}
		}
	})
}

func TestGetOrder_Ownership(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		create := newCreate(repo)
		mine := place(t, create, alice, "")
		get := usecase.NewGetOrder(repo, ownership.New(adminRole))

		for name, ctx := range map[string]context.Context{"owner": as(alice), "admin": as(ops), "anonymous": context.Background()} {
			if out, err := get.Execute(ctx, dto.GetOrderInput{OrderID: mine.ID}); err != nil || out.ID != mine.ID {
				t.Fatalf("%s get = %+v, %v", name, out, err)
			}
		}
		// Somebody else's order looks exactly like one that does not exist.
		_, other := get.Execute(as(bob), dto.GetOrderInput{OrderID: mine.ID})
		_, missing := get.Execute(as(bob), dto.GetOrderInput{OrderID: "no-such-order"})
		if !errors.Is(other, order.ErrNotFound) || !errors.Is(missing, order.ErrNotFound) {
			t.Fatalf("other = %v, missing = %v, want both ErrNotFound", other, missing)
		}
		if other.Error() != missing.Error() {
			t.Fatalf("other %q differs from missing %q", other, missing)
		}
	})
}
