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
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/invite"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

// Bootstrap makes the first account and its first key in one step, so a
// fresh deployment can open IdentityService without a static auth.api_keys
// entry. It only acts on an empty store: when any account exists it writes
// nothing and reports Created false. Runs inside one transaction
// (decorateTx), so a failure half-way leaves no account without a key.
// The guard is a read, not a constraint: two runs racing on an empty store
// can both create — run it once, from one place (a job, an operator).
type Bootstrap struct {
	accounts account.Repository
	keys     apikey.Repository
	invites  invite.Repository
	ids      port.IDGenerator
	secrets  port.SecretGenerator
	clock    port.Clock
}

func NewBootstrap(accounts account.Repository, keys apikey.Repository, invites invite.Repository, ids port.IDGenerator, secrets port.SecretGenerator, clock port.Clock) *Bootstrap {
	return &Bootstrap{accounts: accounts, keys: keys, invites: invites, ids: ids, secrets: secrets, clock: clock}
}

func (uc *Bootstrap) Execute(c context.Context, in dto.BootstrapInput) (*dto.BootstrapOutput, error) {
	ctx := contextx.FromContext(c)

	// Validated first, so a bad address writes nothing whatever the store holds.
	if in.AdminEmail != "" {
		if _, err := invite.NormalizeEmail(in.AdminEmail); err != nil {
			return nil, err
		}
	}
	existing, _, err := uc.accounts.List(ctx, account.ListParams{Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	if len(existing) > 0 {
		return &dto.BootstrapOutput{Created: false}, nil
	}

	now := uc.clock.Now()
	accountID, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate account id: %w", err)
	}
	a, err := account.New(account.ID(accountID), in.Name, in.Roles, now)
	if err != nil {
		return nil, err
	}
	if in.AdminEmail != "" {
		return uc.withInvite(ctx, a, in.AdminEmail, now)
	}
	keyID, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate api key id: %w", err)
	}
	secret, err := uc.secrets.NewSecret(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate api key secret: %w", err)
	}
	k, err := apikey.New(apikey.ID(keyID), accountID, in.KeyName, secret, in.ExpiresAt, now)
	if err != nil {
		return nil, err
	}
	// Both aggregates are validated before either is written.
	if err := uc.accounts.Add(ctx, a); err != nil {
		return nil, fmt.Errorf("add account %s: %w", accountID, err)
	}
	if err := uc.keys.Add(ctx, k); err != nil {
		return nil, fmt.Errorf("add api key %s: %w", keyID, err)
	}

	return &dto.BootstrapOutput{Created: true, Account: *mapper.ToAccountOutput(a), Key: *mapper.ToAPIKeyOutput(k), Secret: secret}, nil
}

// withInvite is the OIDC path (`--admin-email`): the admin account and an
// invite for that email instead of an API key. There is no secret to hand out;
// the deployer signs in at the IdP and claims the invite (ClaimInvite).
func (uc *Bootstrap) withInvite(ctx context.Context, a *account.Account, email string, now time.Time) (*dto.BootstrapOutput, error) {
	inviteID, err := uc.ids.NewID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate invite id: %w", err)
	}
	v, err := invite.New(invite.ID(inviteID), email, a.ID().String(), now)
	if err != nil {
		return nil, err
	}
	if err := uc.accounts.Add(ctx, a); err != nil {
		return nil, fmt.Errorf("add account %s: %w", a.ID(), err)
	}
	if err := uc.invites.Add(ctx, v); err != nil {
		return nil, fmt.Errorf("add invite %s: %w", inviteID, err)
	}

	return &dto.BootstrapOutput{Created: true, Account: *mapper.ToAccountOutput(a), Invite: *toInviteOutput(v)}, nil
}
