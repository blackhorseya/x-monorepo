package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ invite.Repository = (*InviteRepository)(nil)

// InviteRepository keeps copies like the other memory repositories. It takes
// no row lock: the memory Transactor serialises writers, which is what makes
// find → claim → save atomic here.
type InviteRepository struct {
	mu   sync.RWMutex
	data []*invite.Invite
}

func NewInviteRepository() *InviteRepository { return &InviteRepository{} }

func cloneInvite(v *invite.Invite, version int64) (*invite.Invite, error) {
	return invite.Reconstitute(invite.ReconstituteParams{
		ID: v.ID(), Email: v.Email(), AccountID: v.AccountID(), CreatedAt: v.CreatedAt(),
		ExpiresAt: v.ExpiresAt(), ClaimedAt: v.ClaimedAt(), Version: version,
	})
}

func (i *InviteRepository) Add(c context.Context, v *invite.Invite) error {
	if err := contextx.FromContext(c).Err(); err != nil {
		return err
	}
	cp, err := cloneInvite(v, 1)
	if err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.data = append(i.data, cp)

	return nil
}

func (i *InviteRepository) FindLatestByEmail(c context.Context, email string) (*invite.Invite, error) {
	if err := contextx.FromContext(c).Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()
	var best *invite.Invite
	for _, v := range i.data {
		if v.Email() != email {
			continue
		}
		if best == nil || !v.CreatedAt().Before(best.CreatedAt()) {
			best = v
		}
	}
	if best == nil {
		return nil, invite.ErrNotFound
	}

	return cloneInvite(best, best.Version())
}

func (i *InviteRepository) Save(c context.Context, v *invite.Invite) error {
	if err := contextx.FromContext(c).Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	for idx, cur := range i.data {
		if cur.ID() != v.ID() {
			continue
		}
		if cur.Version() != v.Version() {
			return invite.ErrStale
		}
		cp, err := cloneInvite(v, v.Version()+1)
		if err != nil {
			return err
		}
		i.data[idx] = cp

		return nil
	}

	return invite.ErrNotFound
}

func (i *InviteRepository) ListByAccount(c context.Context, accountID string) ([]*invite.Invite, error) {
	if err := contextx.FromContext(c).Err(); err != nil {
		return nil, err
	}

	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]*invite.Invite, 0)
	for _, v := range i.data {
		if v.AccountID() != accountID {
			continue
		}
		cp, err := cloneInvite(v, v.Version())
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	slices.SortFunc(out, func(a, b *invite.Invite) int {
		return cmp.Or(b.CreatedAt().Compare(a.CreatedAt()), cmp.Compare(b.ID(), a.ID()))
	})

	return out, nil
}
