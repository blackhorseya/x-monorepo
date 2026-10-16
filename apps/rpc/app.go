// apps/rpc is a composition root: load config → wire → mount each BC's rpc
// adapter → run. No business logic, no mapping.
//
// Adding a BC: add its *Service to NewRoutes and its ProviderSet to wire.go.
//
// Consumer adapters are NOT mounted here. When a second process is needed
// (scale by queue depth instead of RPS), create apps/consumer with its own
// wire.go that wires the same BC ProviderSets and subscribes e.g.
// consumer.OrderCancellationHandler to consumer.Topic. Until then, mounting a
// subscriber in this process is also fine (go-micro style) — that is an apps/
// decision, the BC does not care.
package main

import (
	orderingrpc "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewRoutes lists every mounted service.
func NewRoutes(ordering *orderingrpc.OrderService) []server.Route {
	return []server.Route{ordering}
}

// NewDocs lists every BC's OpenAPI document (served only when APP_HTTP_DOCS=true).
func NewDocs() []server.Doc {
	return []server.Doc{{Name: "ordering", Spec: orderingv1.OpenAPI}}
}
