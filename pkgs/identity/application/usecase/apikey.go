package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/mapper"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// CreateAPIKey mints a key for an existing account and returns the secret once.
// A disabled account can still get keys (they start working on Enable).
type CreateAPIKey struct {
	accounts account.Repository
	keys     apikey.Repository
	ids      port.IDGenerator
	secrets  port.SecretGenerator
	clock    port.Clock
}

func NewCreateAPIKey(accounts account.Repository, keys apikey.Repository, ids port.IDGenerator, secrets port.SecretGenerator, clock port.Clock) *CreateAPIKey {
	return &CreateAPIKey{accounts: accounts, keys: keys, ids: ids, secrets: secrets, clock: clock}
}

func (uc *CreateAPIKey) Execute(c context.Context, in dto.CreateAPIKeyInput) (*dto.CreateAPIKeyOutput, error) {
	ctx := contextx.FromContext(c)
	if in.AccountID == "" {
		return nil, apikey.ErrInvalidAccountID
	}
	if _, err := uc.accounts.FindByID(ctx, account.ID(in.AccountID)); err != nil {
		return nil, err
	}
	id, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate api key id: %w", err)
	}
	secret, err := uc.secrets.NewSecret(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate api key secret: %w", err)
	}
	k, err := apikey.New(apikey.ID(id), in.AccountID, in.Name, secret, in.ExpiresAt, uc.clock.Now())
	if err != nil {
		return nil, err
	}
	if err := uc.keys.Add(ctx, k); err != nil {
		return nil, fmt.Errorf("add api key %s: %w", id, err)
	}

	return &dto.CreateAPIKeyOutput{Key: *mapper.ToAPIKeyOutput(k), Secret: secret}, nil
}

// ListAPIKeys lists an account's keys, revoked ones included.
type ListAPIKeys struct {
	accounts account.Repository
	keys     apikey.Repository
}

func NewListAPIKeys(accounts account.Repository, keys apikey.Repository) *ListAPIKeys {
	return &ListAPIKeys{accounts: accounts, keys: keys}
}

func (uc *ListAPIKeys) Execute(c context.Context, in dto.ListAPIKeysInput) (*dto.ListAPIKeysOutput, error) {
	ctx := contextx.FromContext(c)
	if in.AccountID == "" {
		return nil, apikey.ErrInvalidAccountID
	}
	if _, err := uc.accounts.FindByID(ctx, account.ID(in.AccountID)); err != nil {
		return nil, err
	}
	keys, err := uc.keys.ListByAccount(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	out := &dto.ListAPIKeysOutput{Keys: make([]dto.APIKeyOutput, 0, len(keys))}
	for _, k := range keys {
		out.Keys = append(out.Keys, *mapper.ToAPIKeyOutput(k))
	}

	return out, nil
}

// RevokeAPIKey ends a key for good. Revoking the last usable key of the last
// effective admin is account.ErrLastAdmin.
type RevokeAPIKey struct {
	keys   apikey.Repository
	census port.AdminCensus
	role   string
	clock  port.Clock
}

func NewRevokeAPIKey(keys apikey.Repository, census port.AdminCensus, adminRole string, clock port.Clock) *RevokeAPIKey {
	return &RevokeAPIKey{keys: keys, census: census, role: adminRole, clock: clock}
}

func (uc *RevokeAPIKey) Execute(c context.Context, in dto.RevokeAPIKeyInput) (*dto.APIKeyOutput, error) {
	ctx := contextx.FromContext(c)
	if in.ID == "" {
		return nil, apikey.ErrInvalidID
	}
	g := &adminGuard{census: uc.census, role: uc.role}
	admins, now, err := g.lock(ctx)
	if err != nil {
		return nil, err
	}
	k, err := uc.keys.FindByID(ctx, apikey.ID(in.ID))
	if err != nil {
		return nil, err
	}
	// Only a key that resolves today counts towards its account's standing;
	// revoking an expired one changes nothing.
	usable := k.Usable(now) == nil
	if err := k.Revoke(now); err != nil {
		return nil, err
	}
	if usable {
		if err := account.GuardLastAdminKey(admins, account.ID(k.AccountID())); err != nil {
			return nil, err
		}
	}
	if err := uc.keys.Save(ctx, k); err != nil {
		return nil, fmt.Errorf("save api key %s: %w", in.ID, err)
	}

	return mapper.ToAPIKeyOutput(k), nil
}

// ResolveAPIKey is the auth path: a presented secret to the account it speaks
// for. Every reason it does not resolve is a domain error the adapter maps to
// one "invalid credential" — the caller never learns which.
type ResolveAPIKey struct {
	accounts account.Repository
	keys     apikey.Repository
	clock    port.Clock
}

func NewResolveAPIKey(accounts account.Repository, keys apikey.Repository, clock port.Clock) *ResolveAPIKey {
	return &ResolveAPIKey{accounts: accounts, keys: keys, clock: clock}
}

func (uc *ResolveAPIKey) Execute(c context.Context, in dto.ResolveAPIKeyInput) (*dto.ResolveAPIKeyOutput, error) {
	ctx := contextx.FromContext(c)
	if len(in.Secret) < apikey.MinSecretLen {
		return nil, apikey.ErrInvalidSecret
	}
	k, err := uc.keys.FindByHash(ctx, apikey.HashSecret(in.Secret))
	if err != nil {
		return nil, err
	}
	if err := k.Usable(uc.clock.Now()); err != nil {
		return nil, err
	}
	a, err := uc.accounts.FindByID(ctx, account.ID(k.AccountID()))
	if err != nil {
		return nil, err
	}
	if !a.IsActive() {
		return nil, account.ErrDisabled
	}

	return &dto.ResolveAPIKeyOutput{AccountID: a.ID().String(), Name: a.Name(), Roles: a.Roles(), KeyID: k.ID().String(), ExpiresAt: k.ExpiresAt()}, nil
}

// TouchAPIKey stamps last_used_at on a key that just resolved, at most once per
// minInterval (the repository decides, atomically, so replicas agree). It is
// the write behind the resolver's throttle, which already keeps requests away
// from it; the condition here is what holds across replicas and restarts.
type TouchAPIKey struct {
	keys        apikey.Repository
	clock       port.Clock
	minInterval time.Duration
}

func NewTouchAPIKey(keys apikey.Repository, clock port.Clock, minInterval time.Duration) *TouchAPIKey {
	return &TouchAPIKey{keys: keys, clock: clock, minInterval: minInterval}
}

func (uc *TouchAPIKey) Execute(c context.Context, in dto.TouchAPIKeyInput) (*dto.TouchAPIKeyOutput, error) {
	ctx := contextx.FromContext(c)
	if in.KeyID == "" {
		return nil, apikey.ErrInvalidID
	}
	recorded, err := uc.keys.TouchLastUsed(ctx, apikey.ID(in.KeyID), uc.clock.Now(), uc.minInterval)
	if err != nil {
		return nil, fmt.Errorf("touch api key %s: %w", in.KeyID, err)
	}

	return &dto.TouchAPIKeyOutput{Recorded: recorded}, nil
}
