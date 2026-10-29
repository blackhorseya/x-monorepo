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
	"context"

	"gorm.io/gorm"

	catalogrpc "github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/rpc"
	catalogv1 "github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1"
	identityrpc "github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/rpc"
	identityv1 "github.com/blackhorseya/x-monorepo/pkgs/identity/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory"
	inventoryrpc "github.com/blackhorseya/x-monorepo/pkgs/inventory/adapters/rpc"
	inventoryv1 "github.com/blackhorseya/x-monorepo/pkgs/inventory/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering"
	orderingrpc "github.com/blackhorseya/x-monorepo/pkgs/ordering/adapters/rpc"
	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	adminv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/admin/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// NewRoutes lists every mounted service: the BCs' and the platform's admin surface.
func NewRoutes(ordering *orderingrpc.OrderService, inventory *inventoryrpc.InventoryService, identity *identityrpc.IdentityService, catalog *catalogrpc.CatalogService, poison *admin.PoisonService) []server.Route {
	return []server.Route{ordering, inventory, identity, catalog, poison}
}

// NewPoisonService serves admin.v1.AdminService over every BC's poison
// table. Adding a BC: add its Schema here (the worker's poison.go lists the
// same set). identity and catalog are not here: they consume no events, so
// they have no poison table.
func NewPoisonService(db *gorm.DB, auth config.Auth) *admin.PoisonService {
	return admin.NewPoisonService(db, auth, []string{ordering.Schema, inventory.Schema})
}

// NewReadiness lists what /api/readyz checks: the database when one is
// configured (nil db = in-memory repositories, nothing to probe).
func NewReadiness(db *gorm.DB) server.Readiness {
	if db == nil {
		return nil
	}
	return server.Readiness{{Name: "postgres", Probe: func(c context.Context) error {
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		return sqlDB.PingContext(c)
	}}}
}

// NewDocs lists every BC's OpenAPI document (served only when APP_HTTP_DOCS=true).
func NewDocs() []server.Doc {
	return []server.Doc{
		{Name: "ordering", Spec: orderingv1.OpenAPI},
		{Name: "inventory", Spec: inventoryv1.OpenAPI},
		{Name: "identity", Spec: identityv1.OpenAPI},
		{Name: "catalog", Spec: catalogv1.OpenAPI},
		{Name: "admin", Spec: adminv1.OpenAPI},
	}
}
