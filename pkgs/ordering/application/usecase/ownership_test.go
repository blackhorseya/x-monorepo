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

// spyRepo records which find the use case chose. GetOrder for a confined
// caller must go through FindByIDForCustomer and never FindByID: FindByID
// loads every item before the ownership check, which makes a foreign order
// measurably slower than a missing one.
type spyRepo struct {
	order.Repository
	findByID, findForCustomer []string
}

func (r *spyRepo) FindByID(c context.Context, id order.ID) (*order.Order, error) {
	r.findByID = append(r.findByID, string(id))
	return r.Repository.FindByID(c, id)
}

func (r *spyRepo) FindByIDForCustomer(c context.Context, id order.ID, customerID string) (*order.Order, error) {
	r.findForCustomer = append(r.findForCustomer, string(id)+"/"+customerID)
	return r.Repository.FindByIDForCustomer(c, id, customerID)
}

func TestGetOrder_ConfinedCallerNeverLoadsAForeignOrder(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		mine := place(t, newCreate(repo), alice, "")
		spy := &spyRepo{Repository: repo}
		get := usecase.NewGetOrder(spy, ownership.New(adminRole))

		// bob asks for alice's order and for one that does not exist: same path, same answer.
		for _, id := range []string{mine.ID, "no-such-order"} {
			if _, err := get.Execute(as(bob), dto.GetOrderInput{OrderID: id}); !errors.Is(err, order.ErrNotFound) {
				t.Fatalf("bob get %s err = %v, want ErrNotFound", id, err)
			}
		}
		if len(spy.findByID) != 0 {
			t.Fatalf("FindByID called for a confined caller: %v", spy.findByID)
		}
		if want := []string{mine.ID + "/" + bob.Subject, "no-such-order/" + bob.Subject}; len(spy.findForCustomer) != 2 || spy.findForCustomer[0] != want[0] || spy.findForCustomer[1] != want[1] {
			t.Fatalf("FindByIDForCustomer calls = %v, want %v", spy.findForCustomer, want)
		}

		// The owner still reads it; admin and anonymous keep the plain find.
		spy.findByID, spy.findForCustomer = nil, nil
		if _, err := get.Execute(as(alice), dto.GetOrderInput{OrderID: mine.ID}); err != nil || len(spy.findForCustomer) != 1 || len(spy.findByID) != 0 {
			t.Fatalf("owner: err %v, forCustomer %v, byID %v", err, spy.findForCustomer, spy.findByID)
		}
		spy.findByID, spy.findForCustomer = nil, nil
		for name, ctx := range map[string]context.Context{"admin": as(ops), "anonymous": context.Background()} {
			if _, err := get.Execute(ctx, dto.GetOrderInput{OrderID: mine.ID}); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if len(spy.findByID) != 2 || len(spy.findForCustomer) != 0 {
			t.Fatalf("admin/anonymous: byID %v, forCustomer %v, want 2 plain finds", spy.findByID, spy.findForCustomer)
		}
	})
}

// The store itself decides ownership.
func TestRepository_FindByIDForCustomer(t *testing.T) {
	repos(t, func(t *testing.T, repo order.Repository) {
		mine := place(t, newCreate(repo), alice, "")
		c := context.Background()

		got, err := repo.FindByIDForCustomer(c, order.ID(mine.ID), alice.Subject)
		if err != nil || got.ID().String() != mine.ID || len(got.Items()) != 1 {
			t.Fatalf("owner = %v, %v", got, err)
		}
		if _, err := repo.FindByIDForCustomer(c, order.ID(mine.ID), bob.Subject); !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("foreign = %v, want ErrNotFound", err)
		}
		if _, err := repo.FindByIDForCustomer(c, "no-such-order", alice.Subject); !errors.Is(err, order.ErrNotFound) {
			t.Fatalf("missing = %v, want ErrNotFound", err)
		}
	})
}
