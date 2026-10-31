// Package catalog is the catalog bounded context: the server-side source of
// product prices (SKU, name, price with its own currency, active). It
// publishes no events and consumes none; ordering prices its orders through its
// QuotePrices RPC. ProviderSet is its only export to
// composition roots (apps/*).
package catalog

import (
	"log/slog"

	"github.com/google/wire"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/client"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/contract/v1/catalogv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/domain/product"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/catalog/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Schema is the Postgres schema of this BC (tables, migrations).
const Schema = postgres.Schema

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	ProvideProductRepository,
	ProvideTransactor,
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),

	// application (decorated; see decorate). *usecasex.Recorder is shared
	// across BCs: apps provide it.
	ProvideUpsertProduct,
	ProvideGetProduct,
	ProvideListProducts,
	ProvideDeactivateProduct,
	ProvideQuotePrices,

	// adapters
	rpc.NewCatalogService,
)

// ClientProviderSet is the catalog as another process's service: the composition root that does not
// own the catalog (apps/rpc) asks catalog-rpc over Connect instead of loading the BC. It provides the
// catalogv1connect.CatalogServiceHandler the other BCs' adapters ask (ordering's pricing, inventory's
// product check) and the Forwarder that serves catalog.v1 on the caller's own port. Needs config.Catalog
// (upstream, service key) and the otel providers; builds nothing of the BC itself, so there is no
// repository, no database and no catalog schema on that side.
var ClientProviderSet = wire.NewSet(
	client.New,
	wire.Bind(new(catalogv1connect.CatalogServiceHandler), new(*client.Client)),
	rpc.NewForwarder,
)

// ProvideProductRepository picks the persistence backend from the composition
// root's database handle: nil (APP_POSTGRES_URL unset) means in-memory.
func ProvideProductRepository(db *gorm.DB, logger *slog.Logger) product.Repository {
	if db == nil {
		logger.Warn("catalog: APP_POSTGRES_URL unset, using in-memory product repository")
		return memory.NewProductRepository()
	}

	return postgres.NewProductRepository(db)
}

// ProvideTransactor pairs with the repository: a real transaction on
// Postgres, a pass-through in memory.
func ProvideTransactor(db *gorm.DB) port.Transactor {
	if db == nil {
		return memory.NewTransactor()
	}

	return postgres.NewTransactor(db)
}

// decorate is the one place that decides which cross-cutting decorators every
// use case of this BC gets (outermost first). Reads use it as is.
func decorate[I, O any](name string, rec *usecasex.Recorder, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
	)
}

// decorateTx is decorate for use cases that write: one transaction around
// the use case. This BC has no consumer path, so there is no inbox / Idempotent.
func decorateTx[I, O any](name string, rec *usecasex.Recorder, tx port.Transactor, uc usecasex.UseCase[I, O]) usecasex.UseCase[I, O] {
	return usecasex.Chain(uc,
		usecasex.Metrics[I, O](rec, name),
		usecasex.Logging[I, O](name),
		usecasex.Transactional[I, O](tx),
	)
}

func ProvideUpsertProduct(products product.Repository, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.UpsertProductUC {
	return decorateTx("catalog.upsert_product", rec, tx, usecase.NewUpsertProduct(products, clock))
}

func ProvideGetProduct(products product.Repository, rec *usecasex.Recorder) rpc.GetProductUC {
	return decorate("catalog.get_product", rec, usecase.NewGetProduct(products))
}

func ProvideListProducts(products product.Repository, rec *usecasex.Recorder) rpc.ListProductsUC {
	return decorate("catalog.list_products", rec, usecase.NewListProducts(products))
}

func ProvideDeactivateProduct(products product.Repository, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.DeactivateProductUC {
	return decorateTx("catalog.deactivate_product", rec, tx, usecase.NewDeactivateProduct(products, clock))
}

// ProvideQuotePrices is the batch price lookup other bounded contexts ask for through
// CatalogService.QuotePrices (ordering's adapters/pricing, in process).
func ProvideQuotePrices(products product.Repository, rec *usecasex.Recorder) rpc.QuotePricesUC {
	return decorate("catalog.quote_prices", rec, usecase.NewQuotePrices(products))
}
