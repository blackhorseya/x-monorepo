package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	platformpg "github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

var _ port.AdminCensus = (*AdminCensus)(nil)

// AdminCensus is the Postgres census. It is two statements on purpose: the
// first locks (FOR UPDATE, by id so two guards take the rows in the same
// order) the ACTIVE accounts that hold the role; the second counts their
// usable keys. One statement with an EXISTS on api_keys would re-check only
// the locked account rows after waiting, and keep reading api_keys from its
// original snapshot; a statement started after the lock sees what the
// transaction that held it committed.
type AdminCensus struct {
	db    *gorm.DB
	clock port.Clock
}

func NewAdminCensus(db *gorm.DB, clock port.Clock) *AdminCensus {
	return &AdminCensus{db: db, clock: clock}
}

func (i *AdminCensus) LockAdmins(c context.Context, role string) ([]account.AdminStanding, time.Time, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return nil, time.Time{}, err
	}

	// jsonb containment; the array literal is a bound parameter, so the `?` of
	// the operator family never meets gorm's placeholder.
	want, err := json.Marshal([]string{role})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("encode role %q: %w", role, err)
	}
	tx := platformpg.TxOrDB(ctx, i.db)
	var ids []string
	if err := tx.Model(&accountRow{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("status = ? AND roles @> ?::jsonb", account.StatusActive.String(), string(want)).
		Order("id ASC").
		Pluck("id", &ids).Error; err != nil {
		return nil, time.Time{}, fmt.Errorf("lock admin accounts: %w", err)
	}
	// Read the clock only now: the first statement may have queued behind another
	// writer, and a key that expired meanwhile must not count.
	now := i.clock.Now()
	if len(ids) == 0 {
		return nil, now, nil
	}

	var counts []struct {
		AccountID string
		N         int
	}
	if err := tx.Model(&apiKeyRow{}).
		Select("account_id, count(*) AS n").
		Where("account_id IN ? AND status = ? AND (expires_at IS NULL OR expires_at > ?)", ids, apikey.StatusActive.String(), now).
		Group("account_id").
		Scan(&counts).Error; err != nil {
		return nil, time.Time{}, fmt.Errorf("count admin keys: %w", err)
	}
	n := make(map[string]int, len(counts))
	for _, row := range counts {
		n[row.AccountID] = row.N
	}
	out := make([]account.AdminStanding, 0, len(ids))
	for _, id := range ids {
		if n[id] > 0 {
			out = append(out, account.NewAdminStanding(account.ID(id), n[id]))
		}
	}

	return out, now, nil
}
