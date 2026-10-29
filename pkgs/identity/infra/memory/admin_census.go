package memory

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ port.AdminCensus = (*AdminCensus)(nil)

// AdminCensus counts the effective admins through the two repositories. It
// takes no lock of its own: the memory Transactor serialises whole writers,
// which is what makes census → decide → save atomic here.
type AdminCensus struct {
	accounts account.Repository
	keys     apikey.Repository
}

func NewAdminCensus(accounts account.Repository, keys apikey.Repository) *AdminCensus {
	return &AdminCensus{accounts: accounts, keys: keys}
}

const censusPage = 100

func (i *AdminCensus) LockAdmins(c context.Context, role string, now time.Time) ([]account.AdminStanding, error) {
	ctx := contextx.FromContext(c)

	var out []account.AdminStanding
	var after account.ID
	for {
		page, next, err := i.accounts.List(ctx, account.ListParams{Limit: censusPage, After: after})
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			if !a.IsActive() || !a.HasRole(role) {
				continue
			}
			keys, err := i.keys.ListByAccount(ctx, a.ID().String())
			if err != nil {
				return nil, err
			}
			usable := 0
			for _, k := range keys {
				if k.Usable(now) == nil {
					usable++
				}
			}
			if usable > 0 {
				out = append(out, account.NewAdminStanding(a.ID(), usable))
			}
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}
