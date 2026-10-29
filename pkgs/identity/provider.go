// Package identity is the identity bounded context: the accounts and API keys
// the rpc server's auth interceptor resolves X-API-Key against (next to the
// static auth.api_keys, which stay the break-glass path). It only verifies —
// JWTs keep coming from the external issuer of auth.jwt — and it publishes no
// events. ProviderSet is its only export to composition roots (apps/*); the
// platform reaches it through server.CredentialResolver (adapters/auth), wired
// in apps/rpc.
package identity

import (
	"log/slog"
	"time"

	"github.com/google/wire"
	"go.opentelemetry.io/otel/metric"
	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/auth"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/cli"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/adapters/rpc"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/postgres"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/system"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

// Schema is the Postgres schema of this BC (tables, migrations).
const Schema = postgres.Schema

var ProviderSet = wire.NewSet(
	// infra → ports / repositories
	ProvideAccountRepository,
	ProvideAPIKeyRepository,
	ProvideTransactor,
	ProvideAdminCensus,
	system.NewClock,
	wire.Bind(new(port.Clock), new(*system.Clock)),
	system.NewUUIDGenerator,
	wire.Bind(new(port.IDGenerator), new(*system.UUIDGenerator)),
	system.NewSecretGenerator,
	wire.Bind(new(port.SecretGenerator), new(*system.SecretGenerator)),

	// application (decorated; see decorate). *usecasex.Recorder is shared
	// across BCs: apps provide it.
	ProvideCreateAccount,
	ProvideGetAccount,
	ProvideListAccounts,
	ProvideDisableAccount,
	ProvideEnableAccount,
	ProvideAssignRoles,
	ProvideCreateAPIKey,
	ProvideListAPIKeys,
	ProvideRevokeAPIKey,
	ProvideResolveAPIKey,
	ProvideTouchAPIKey,
	ProvideWhoAmI,
	ProvideBootstrap,
	wire.Struct(new(rpc.UseCases), "*"),

	// adapters
	rpc.NewIdentityService,
	ProvideResolver,
	cli.NewBootstrap,
)

// ProvideAccountRepository picks the persistence backend from the composition
// root's database handle: nil (APP_POSTGRES_URL unset) means in-memory.
func ProvideAccountRepository(db *gorm.DB, logger *slog.Logger) account.Repository {
	if db == nil {
		logger.Warn("identity: APP_POSTGRES_URL unset, using in-memory account repository")
		return memory.NewAccountRepository()
	}

	return postgres.NewAccountRepository(db)
}

func ProvideAPIKeyRepository(db *gorm.DB) apikey.Repository {
	if db == nil {
		return memory.NewAPIKeyRepository()
	}

	return postgres.NewAPIKeyRepository(db)
}

// ProvideTransactor pairs with the repositories: a real transaction on
// Postgres, a pass-through in memory.
func ProvideTransactor(db *gorm.DB) port.Transactor {
	if db == nil {
		return memory.NewTransactor()
	}

	return postgres.NewTransactor(db)
}

// ProvideAdminCensus pairs with the repositories too: Postgres locks the admin
// rows for the transaction, in memory the Transactor serialises writers.
func ProvideAdminCensus(db *gorm.DB, accounts account.Repository, keys apikey.Repository) port.AdminCensus {
	if db == nil {
		return memory.NewAdminCensus(accounts, keys)
	}

	return postgres.NewAdminCensus(db)
}

// adminRole is the role the last-admin guard protects: auth.admin_role, the
// same one IdentityService requires of its callers.
func adminRole(auth config.Auth) string {
	if auth.AdminRole == "" {
		return admin.DefaultAdminRole // config.Load sets the same; this is for hand-built configs (tests)
	}

	return auth.AdminRole
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

func ProvideCreateAccount(accounts account.Repository, ids port.IDGenerator, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.CreateAccountUC {
	return decorateTx("identity.create_account", rec, tx, usecase.NewCreateAccount(accounts, ids, clock))
}

func ProvideGetAccount(accounts account.Repository, rec *usecasex.Recorder) rpc.GetAccountUC {
	return decorate("identity.get_account", rec, usecase.NewGetAccount(accounts))
}

func ProvideListAccounts(accounts account.Repository, rec *usecasex.Recorder) rpc.ListAccountsUC {
	return decorate("identity.list_accounts", rec, usecase.NewListAccounts(accounts))
}

func ProvideDisableAccount(accounts account.Repository, census port.AdminCensus, cfg config.Auth, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.DisableAccountUC {
	return decorateTx("identity.disable_account", rec, tx, usecase.NewDisableAccount(accounts, census, adminRole(cfg), clock))
}

func ProvideEnableAccount(accounts account.Repository, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.EnableAccountUC {
	return decorateTx("identity.enable_account", rec, tx, usecase.NewEnableAccount(accounts, clock))
}

func ProvideAssignRoles(accounts account.Repository, census port.AdminCensus, cfg config.Auth, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.AssignRolesUC {
	return decorateTx("identity.assign_roles", rec, tx, usecase.NewAssignRoles(accounts, census, adminRole(cfg), clock))
}

func ProvideCreateAPIKey(accounts account.Repository, keys apikey.Repository, ids port.IDGenerator, secrets port.SecretGenerator, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.CreateAPIKeyUC {
	return decorateTx("identity.create_api_key", rec, tx, usecase.NewCreateAPIKey(accounts, keys, ids, secrets, clock))
}

func ProvideListAPIKeys(accounts account.Repository, keys apikey.Repository, rec *usecasex.Recorder) rpc.ListAPIKeysUC {
	return decorate("identity.list_api_keys", rec, usecase.NewListAPIKeys(accounts, keys))
}

func ProvideRevokeAPIKey(keys apikey.Repository, census port.AdminCensus, cfg config.Auth, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) rpc.RevokeAPIKeyUC {
	return decorateTx("identity.revoke_api_key", rec, tx, usecase.NewRevokeAPIKey(keys, census, adminRole(cfg), clock))
}

// ProvideResolveAPIKey is the auth path (adapters/auth.Resolver, in front of
// the platform's interceptor). Logging's success line is Debug, and the
// resolver caches, so the access log is not doubled per request.
func ProvideResolveAPIKey(accounts account.Repository, keys apikey.Repository, clock port.Clock, rec *usecasex.Recorder) auth.ResolveAPIKeyUC {
	return decorate("identity.resolve_api_key", rec, usecase.NewResolveAPIKey(accounts, keys, clock))
}

// ProvideTouchAPIKey is the write behind auth.last_used_interval: one
// conditional UPDATE per key per window, no transaction (a single statement) and
// no Logging line per call (the resolver logs failures by class, and a success
// is the common case).
func ProvideTouchAPIKey(keys apikey.Repository, clock port.Clock, cfg config.Auth, rec *usecasex.Recorder) auth.TouchAPIKeyUC {
	return usecasex.Chain[dto.TouchAPIKeyInput, *dto.TouchAPIKeyOutput](usecase.NewTouchAPIKey(keys, clock, cfg.LastUsedInterval),
		usecasex.Metrics[dto.TouchAPIKeyInput, *dto.TouchAPIKeyOutput](rec, "identity.touch_api_key"),
	)
}

// resolverDrain bounds how long shutdown waits for the last last_used_at writes
// (each already bounded by the resolver's own timeout).
const resolverDrain = 3 * time.Second

// ProvideResolver is the auth path's resolver with last_used_at switched on as
// configured (auth.last_used_interval, 0 = off). Its cleanup gives the stamps
// still in flight a bounded moment to land; wire runs it before the database
// handle it depends on is closed.
func ProvideResolver(resolve auth.ResolveAPIKeyUC, touch auth.TouchAPIKeyUC, clock port.Clock, cfg config.Auth, logger *slog.Logger, mp metric.MeterProvider) (*auth.Resolver, func(), error) {
	r, err := auth.NewResolver(resolve, clock, mp)
	if err != nil {
		return nil, nil, err
	}
	r.TrackLastUsed(touch, cfg.LastUsedInterval, logger)

	return r, func() { r.WaitTimeout(resolverDrain) }, nil
}

func ProvideWhoAmI(rec *usecasex.Recorder) rpc.WhoAmIUC {
	return decorate("identity.whoami", rec, usecase.NewWhoAmI())
}

// ProvideBootstrap is `worker bootstrap` (adapters/cli): first account + first
// key in one transaction on an empty store.
func ProvideBootstrap(accounts account.Repository, keys apikey.Repository, ids port.IDGenerator, secrets port.SecretGenerator, clock port.Clock, tx port.Transactor, rec *usecasex.Recorder) cli.BootstrapUC {
	return decorateTx("identity.bootstrap", rec, tx, usecase.NewBootstrap(accounts, keys, ids, secrets, clock))
}
