package inventory

import (
	"context"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/inventory/domain/stock"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// requireProduct refuses SetStock for a SKU the catalog does not have. It is a decorator, so
// the use case stays as it was and holds no transaction; ProvideSetStock puts it OUTSIDE
// Transactional, which is the point: the catalog is asked before a database connection is
// taken, so a slow catalog cannot starve the pool. Metrics and Logging wrap it, so a refusal is
// counted and logged like any other result.
//
// A malformed SKU is refused here by the domain's own rule first (stock.invalid_sku), so the
// catalog is only asked about SKUs that could be stocked. Whether the product is on sale is
// not asked: a deactivated product can still be counted and cleared.
func requireProduct(products port.ProductCatalog) usecasex.Decorator[dto.SetStockInput, *dto.StockOutput] {
	return func(next usecasex.UseCase[dto.SetStockInput, *dto.StockOutput]) usecasex.UseCase[dto.SetStockInput, *dto.StockOutput] {
		return usecasex.Func[dto.SetStockInput, *dto.StockOutput](func(c context.Context, in dto.SetStockInput) (*dto.StockOutput, error) {
			if err := stock.ValidateSKU(stock.SKU(in.SKU)); err != nil {
				return nil, err
			}
			ok, err := products.HasProduct(c, in.SKU)
			if err != nil {
				return nil, fmt.Errorf("check product %s: %w", in.SKU, err)
			}
			if !ok {
				return nil, stock.ErrUnknownSKU
			}

			return next.Execute(c, in)
		})
	}
}
