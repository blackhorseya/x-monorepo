package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/ownership"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

// seedOrders stores n orders: ids o-00..o-(n-1), alternating customers c-0/c-1,
// every third one cancelled, created one second apart (o-00 oldest). Two share a
// timestamp so the id tie-breaker is exercised.
func seedOrders(t *testing.T, repo order.Repository, n int) {
	t.Helper()
	c := context.Background()
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	usd, _ := kernel.NewMoney(100, "USD")
	item, _ := order.NewLineItem("SKU", 1, usd)
	for i := range n {
		at := base.Add(time.Duration(i) * time.Second)
		if i == n-1 && n > 1 {
			at = base.Add(time.Duration(i-1) * time.Second) // same instant as the previous one
		}
		o, err := order.New(order.Params{
			ID: order.ID(fmt.Sprintf("o-%02d", i)), CustomerID: fmt.Sprintf("c-%d", i%2),
			Items: []order.LineItem{item}, Now: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 2 {
			if err := o.Cancel(at); err != nil {
				t.Fatal(err)
			}
		}
		if err := repo.Save(c, o); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListOrders_WalksAllPagesNewestFirst(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	seedOrders(t, repo, 7)
	list := usecase.NewListOrders(repo, ownership.New("admin"))

	var ids []string
	token := ""
	pages := 0
	for {
		out, err := list.Execute(c, dto.ListOrdersInput{PageSize: 3, PageToken: token})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, o := range out.Orders {
			ids = append(ids, o.ID)
		}
		if out.NextPageToken == "" {
			break
		}
		token = out.NextPageToken
	}
	// o-06 shares its timestamp with o-05: the id tie-breaker puts o-06 first.
	want := []string{"o-06", "o-05", "o-04", "o-03", "o-02", "o-01", "o-00"}
	if pages != 3 || fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("pages=%d ids=%v, want 3 pages %v", pages, ids, want)
	}
}

func TestListOrders_Filters(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	seedOrders(t, repo, 7)
	list := usecase.NewListOrders(repo, ownership.New("admin"))

	out, err := list.Execute(c, dto.ListOrdersInput{CustomerID: "c-1", Status: "CANCELLED"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Orders) != 1 || out.Orders[0].ID != "o-05" || out.NextPageToken != "" {
		t.Fatalf("got %+v", out)
	}

	out, err = list.Execute(c, dto.ListOrdersInput{Status: "PENDING", PageSize: 100})
	if err != nil || len(out.Orders) != 5 {
		t.Fatalf("pending: n=%d err=%v", len(out.Orders), err)
	}
}

func TestListOrders_Rejects(t *testing.T) {
	c := context.Background()
	repo := memory.NewOrderRepository()
	seedOrders(t, repo, 4)
	list := usecase.NewListOrders(repo, ownership.New("admin"))

	first, err := list.Execute(c, dto.ListOrdersInput{CustomerID: "c-0", PageSize: 1})
	if err != nil || first.NextPageToken == "" {
		t.Fatalf("first page: %+v, %v", first, err)
	}

	tests := []struct {
		name string
		in   dto.ListOrdersInput
		want error
	}{
		{"negative page size", dto.ListOrdersInput{PageSize: -1}, pagination.ErrInvalidPageSize},
		{"unknown status", dto.ListOrdersInput{Status: "SHIPPED"}, order.ErrInvalidStatusFilter},
		{"garbage token", dto.ListOrdersInput{PageToken: "!!"}, pagination.ErrInvalidToken},
		{"token replayed with other filters", dto.ListOrdersInput{CustomerID: "c-1", PageToken: first.NextPageToken}, pagination.ErrInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := list.Execute(c, tt.in); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}

	// Page size above the maximum is clamped, not rejected.
	if _, err := list.Execute(c, dto.ListOrdersInput{PageSize: usecase.MaxPageSize + 1}); err != nil {
		t.Fatalf("oversized page: %v", err)
	}
}
