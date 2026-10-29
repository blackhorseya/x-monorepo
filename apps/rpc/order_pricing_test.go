package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1/orderingv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

func startPricing(t *testing.T) (string, orderingv1connect.OrderServiceClient, catalogv1connect.CatalogServiceClient) {
	t.Helper()
	srv, cleanup, err := Initialize(&config.AppConfig{
		HTTP: config.HTTP{Addr: ":0"},
		Log:  config.Log{Level: "error", Format: "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts.URL, orderingv1connect.NewOrderServiceClient(http.DefaultClient, ts.URL), catalogv1connect.NewCatalogServiceClient(http.DefaultClient, ts.URL)
}

func post(t *testing.T, url, contentType string, body []byte) (int, []byte) {
	t.Helper()
	res, err := http.Post(url, contentType, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)

	return res.StatusCode, out
}

// A client written before server pricing keeps sending unit_price (field 3 of a line). The
// contract reserves that number and name, so the field is an unknown one: connect-go's binary
// codec keeps it aside unread and its JSON codec discards it. Either way the call succeeds and
// the price on the order is the catalog's — the caller has no way to set one.
func TestCreateOrder_OldClientsUnitPriceIsIgnored(t *testing.T) {
	base, orders, _ := startPricing(t)
	seedProduct(t, base, "", "A", 1299, "USD")
	const path = "/ordering.v1.OrderService/CreateOrder"

	t.Run("json, snake_case", func(t *testing.T) {
		status, body := post(t, base+path, "application/json",
			[]byte(`{"customer_id":"old-json","items":[{"sku":"A","quantity":2,"unit_price":{"amount_minor":1,"currency":"USD"}}]}`))
		assertPriced(t, status, body, "application/json")
	})
	t.Run("json, camelCase", func(t *testing.T) {
		status, body := post(t, base+path, "application/json",
			[]byte(`{"customerId":"old-json","items":[{"sku":"A","quantity":2,"unitPrice":{"amountMinor":"1","currency":"USD"}}]}`))
		assertPriced(t, status, body, "application/json")
	})
	t.Run("binary, field 3 on the wire", func(t *testing.T) {
		money, err := proto.Marshal(&commonv1.Money{AmountMinor: 1, Currency: "USD"})
		if err != nil {
			t.Fatal(err)
		}
		var item []byte
		item = protowire.AppendTag(item, 1, protowire.BytesType)
		item = protowire.AppendString(item, "A")
		item = protowire.AppendTag(item, 2, protowire.VarintType)
		item = protowire.AppendVarint(item, 2)
		item = protowire.AppendTag(item, 3, protowire.BytesType) // the retired unit_price
		item = protowire.AppendBytes(item, money)
		var req []byte
		req = protowire.AppendTag(req, 1, protowire.BytesType)
		req = protowire.AppendString(req, "old-binary")
		req = protowire.AppendTag(req, 2, protowire.BytesType)
		req = protowire.AppendBytes(req, item)

		status, body := post(t, base+path, "application/proto", req)
		assertPriced(t, status, body, "application/proto")
	})

	// And the order says so when it is read back.
	list, err := orders.ListOrders(context.Background(), connect.NewRequest(&orderingv1.ListOrdersRequest{}))
	if err != nil || len(list.Msg.GetOrders()) != 3 {
		t.Fatalf("orders = %v, %v; want the three above", list, err)
	}
	for _, o := range list.Msg.GetOrders() {
		if o.GetItems()[0].GetUnitPrice().GetAmountMinor() != 1299 || o.GetTotal().GetAmountMinor() != 2598 {
			t.Fatalf("order %s = %v, want the catalog price 1299 x 2", o.GetId(), o)
		}
	}
}

func assertPriced(t *testing.T, status int, body []byte, contentType string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s; an old client's unit_price must not break the call", status, body)
	}
	var order *orderingv1.Order
	if contentType == "application/json" {
		var res struct {
			Order struct {
				Total struct {
					AmountMinor string `json:"amountMinor"`
				} `json:"total"`
				Items []struct {
					UnitPrice struct {
						AmountMinor string `json:"amountMinor"`
					} `json:"unitPrice"`
				} `json:"items"`
			} `json:"order"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			t.Fatal(err)
		}
		if res.Order.Total.AmountMinor != "2598" || len(res.Order.Items) != 1 || res.Order.Items[0].UnitPrice.AmountMinor != "1299" {
			t.Fatalf("response = %s, want 1299 x 2 = 2598 from the catalog", body)
		}

		return
	}
	var res orderingv1.CreateOrderResponse
	if err := proto.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	order = res.GetOrder()
	if order.GetTotal().GetAmountMinor() != 2598 || order.GetItems()[0].GetUnitPrice().GetAmountMinor() != 1299 {
		t.Fatalf("order = %v, want 1299 x 2 = 2598 from the catalog", order)
	}
}

// Through the whole stack: the snapshot survives a reprice, and what the catalog cannot
// quote is refused with 400 before an order exists.
func TestCreateOrder_ServerPricingEndToEnd(t *testing.T) {
	base, orders, catalog := startPricing(t)
	c := context.Background()
	seedProduct(t, base, "", "USD-A", 500, "USD")
	seedProduct(t, base, "", "EUR-A", 400, "EUR")
	seedProduct(t, base, "", "GONE", 100, "USD")
	if _, err := catalog.DeactivateProduct(c, connect.NewRequest(&catalogv1.DeactivateProductRequest{Sku: "GONE"})); err != nil {
		t.Fatal(err)
	}
	place := func(customer string, items ...*orderingv1.CreateOrderItem) (*orderingv1.Order, error) {
		res, err := orders.CreateOrder(c, connect.NewRequest(&orderingv1.CreateOrderRequest{CustomerId: customer, Items: items}))
		if err != nil {
			return nil, err
		}

		return res.Msg.GetOrder(), nil
	}

	placed, err := place("c-1", &orderingv1.CreateOrderItem{Sku: "USD-A", Quantity: 3})
	if err != nil || placed.GetTotal().GetAmountMinor() != 1500 {
		t.Fatalf("placed = %v, %v", placed, err)
	}
	seedProduct(t, base, "", "USD-A", 900, "USD") // reprice
	got, err := orders.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: placed.GetId()}))
	if err != nil || got.Msg.GetOrder().GetTotal().GetAmountMinor() != 1500 || got.Msg.GetOrder().GetItems()[0].GetUnitPrice().GetAmountMinor() != 500 {
		t.Fatalf("after reprice = %v, %v; want the 500 snapshot", got, err)
	}
	next, err := place("c-1", &orderingv1.CreateOrderItem{Sku: "USD-A", Quantity: 1})
	if err != nil || next.GetTotal().GetAmountMinor() != 900 {
		t.Fatalf("next order = %v, %v; want the new price", next, err)
	}

	for name, tc := range map[string]struct {
		items []*orderingv1.CreateOrderItem
		code  string
	}{
		"unknown sku":    {[]*orderingv1.CreateOrderItem{{Sku: "NOPE", Quantity: 1}}, "order.unknown_sku"},
		"deactivated":    {[]*orderingv1.CreateOrderItem{{Sku: "GONE", Quantity: 1}}, "order.inactive_sku"},
		"mixed currency": {[]*orderingv1.CreateOrderItem{{Sku: "USD-A", Quantity: 1}, {Sku: "EUR-A", Quantity: 1}}, "order.mixed_currency"},
	} {
		_, err := place("c-refused", tc.items...)
		wantConnectCode(t, err, connect.CodeInvalidArgument, tc.code)
		_ = name
	}
	list, err := orders.ListOrders(c, connect.NewRequest(&orderingv1.ListOrdersRequest{CustomerId: "c-refused"}))
	if err != nil || len(list.Msg.GetOrders()) != 0 {
		t.Fatalf("refused orders left %v, %v behind", list, err)
	}
}
