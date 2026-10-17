// apps/rpc is a composition root: load config → wire → mount each BC's rpc
// adapter → run. No business logic, no mapping.
//
// Adding a BC: add its *Service to NewRoutes and its ProviderSet to wire.go.
//
// Consumer adapters are NOT mounted here: apps/worker wires the same BC
// ProviderSets and runs their Subscriptions (scale by queue depth instead of
// RPS). Mounting a subscriber in this process would also be fine (go-micro
// style) — that is an apps/ decision, the BC does not care.
package main

import (
	inventoryrpc "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/rpc"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	orderingrpc "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewRoutes lists every mounted service.
func NewRoutes(ordering *orderingrpc.OrderService, inventory *inventoryrpc.InventoryService) []server.Route {
	return []server.Route{ordering, inventory}
}

// NewDocs lists every BC's OpenAPI document (served only when APP_HTTP_DOCS=true).
func NewDocs() []server.Doc {
	return []server.Doc{
		{Name: "ordering", Spec: orderingv1.OpenAPI},
		{Name: "inventory", Spec: inventoryv1.OpenAPI},
	}
}
