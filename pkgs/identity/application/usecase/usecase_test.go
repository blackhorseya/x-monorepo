package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/usecase"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/account"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/domain/apikey"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/infra/memory"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/pagination"
)

type fixedClock struct{ t time.Time }

func (i *fixedClock) Now() time.Time { return i.t }

// seq hands out predictable ids and secrets, in order.
type seq struct {
	n   int
	err error
}

func (s *seq) NewID(context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.n++
	return fmt.Sprintf("id-%02d", s.n), nil
}

func (s *seq) NewSecret(context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.n++
	return fmt.Sprintf("xk_%040d", s.n), nil
}

type fixture struct {
	clock    *fixedClock
	ids      *seq
	secrets  *seq
	accounts *memory.AccountRepository
	keys     *memory.APIKeyRepository

	createAccount  *usecase.CreateAccount
	getAccount     *usecase.GetAccount
	listAccounts   *usecase.ListAccounts
	disableAccount *usecase.DisableAccount
	enableAccount  *usecase.EnableAccount
	assignRoles    *usecase.AssignRoles
	createKey      *usecase.CreateAPIKey
	listKeys       *usecase.ListAPIKeys
	revokeKey      *usecase.RevokeAPIKey
	resolve        *usecase.ResolveAPIKey
}

func newFixture() *fixture {
	f := &fixture{
		clock: &fixedClock{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}, ids: &seq{}, secrets: &seq{n: 100},
		accounts: memory.NewAccountRepository(), keys: memory.NewAPIKeyRepository(),
	}
	f.createAccount = usecase.NewCreateAccount(f.accounts, f.ids, f.clock)
	f.getAccount = usecase.NewGetAccount(f.accounts)
	f.listAccounts = usecase.NewListAccounts(f.accounts)
	f.disableAccount = usecase.NewDisableAccount(f.accounts, f.clock)
	f.enableAccount = usecase.NewEnableAccount(f.accounts, f.clock)
	f.assignRoles = usecase.NewAssignRoles(f.accounts, f.clock)
	f.createKey = usecase.NewCreateAPIKey(f.accounts, f.keys, f.ids, f.secrets, f.clock)
	f.listKeys = usecase.NewListAPIKeys(f.accounts, f.keys)
	f.revokeKey = usecase.NewRevokeAPIKey(f.keys, f.clock)
	f.resolve = usecase.NewResolveAPIKey(f.accounts, f.keys, f.clock)
	return f
}

func TestAccountLifecycle(t *testing.T) {
	f := newFixture()
	c := context.Background()

	a, err := f.createAccount.Execute(c, dto.CreateAccountInput{Name: "ops", Roles: []string{"ops", "admin"}})
	if err != nil || a.ID != "id-01" || a.Status != "ACTIVE" || !slices.Equal(a.Roles, []string{"admin", "ops"}) || !a.CreatedAt.Equal(f.clock.t) {
		t.Fatalf("create = %+v, %v", a, err)
	}
	if _, err := f.createAccount.Execute(c, dto.CreateAccountInput{Name: " "}); !errors.Is(err, account.ErrInvalidName) {
		t.Fatalf("empty name: %v", err)
	}
	f.ids.err = errors.New("entropy exhausted")
	if _, err := f.createAccount.Execute(c, dto.CreateAccountInput{Name: "x"}); err == nil || !strings.Contains(err.Error(), "generate account id") {
		t.Fatalf("id failure: %v", err)
	}
	f.ids.err = nil

	got, err := f.getAccount.Execute(c, dto.GetAccountInput{ID: a.ID})
	if err != nil || got.Name != "ops" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := f.getAccount.Execute(c, dto.GetAccountInput{}); !errors.Is(err, account.ErrInvalidID) {
		t.Fatalf("get empty: %v", err)
	}
	if _, err := f.getAccount.Execute(c, dto.GetAccountInput{ID: "nope"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}

	f.clock.t = f.clock.t.Add(time.Minute)
	d, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: a.ID})
	if err != nil || d.Status != "DISABLED" || !d.UpdatedAt.Equal(f.clock.t) {
		t.Fatalf("disable = %+v, %v", d, err)
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: a.ID}); !errors.Is(err, account.ErrAlreadyDisabled) {
		t.Fatalf("disable twice: %v", err)
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{}); !errors.Is(err, account.ErrInvalidID) {
		t.Fatalf("disable empty: %v", err)
	}
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: "nope"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("disable missing: %v", err)
	}
	e, err := f.enableAccount.Execute(c, dto.EnableAccountInput{ID: a.ID})
	if err != nil || e.Status != "ACTIVE" {
		t.Fatalf("enable = %+v, %v", e, err)
	}
	if _, err := f.enableAccount.Execute(c, dto.EnableAccountInput{ID: a.ID}); !errors.Is(err, account.ErrAlreadyActive) {
		t.Fatalf("enable twice: %v", err)
	}

	r, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: a.ID, Roles: []string{"viewer"}})
	if err != nil || !slices.Equal(r.Roles, []string{"viewer"}) {
		t.Fatalf("assign = %+v, %v", r, err)
	}
	if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: a.ID, Roles: []string{""}}); !errors.Is(err, account.ErrInvalidRole) {
		t.Fatalf("assign bad role: %v", err)
	}

	// Save under a stale version: the repository refuses (ErrStale wraps ErrConcurrentUpdate).
	stale, _ := f.accounts.FindByID(c, account.ID(a.ID))
	if _, err := f.assignRoles.Execute(c, dto.AssignRolesInput{ID: a.ID, Roles: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	_ = stale.AssignRoles([]string{"y"}, f.clock.t)
	if err := f.accounts.Save(c, stale); !errors.Is(err, account.ErrStale) {
		t.Fatalf("stale save: %v", err)
	}
}

func TestListAccounts(t *testing.T) {
	f := newFixture()
	c := context.Background()
	for i := range 5 {
		if _, err := f.createAccount.Execute(c, dto.CreateAccountInput{Name: fmt.Sprintf("a%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.listAccounts.Execute(c, dto.ListAccountsInput{PageSize: 2})
	if err != nil || len(page.Accounts) != 2 || page.Accounts[0].ID != "id-01" || page.NextPageToken == "" {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page2, err := f.listAccounts.Execute(c, dto.ListAccountsInput{PageSize: 2, PageToken: page.NextPageToken})
	if err != nil || len(page2.Accounts) != 2 || page2.Accounts[0].ID != "id-03" {
		t.Fatalf("page 2 = %+v, %v", page2, err)
	}
	page3, err := f.listAccounts.Execute(c, dto.ListAccountsInput{PageSize: 2, PageToken: page2.NextPageToken})
	if err != nil || len(page3.Accounts) != 1 || page3.NextPageToken != "" {
		t.Fatalf("page 3 = %+v, %v", page3, err)
	}
	all, err := f.listAccounts.Execute(c, dto.ListAccountsInput{})
	if err != nil || len(all.Accounts) != 5 || all.NextPageToken != "" {
		t.Fatalf("default page = %+v, %v", all, err)
	}
	if _, err := f.listAccounts.Execute(c, dto.ListAccountsInput{PageSize: -1}); !errors.Is(err, pagination.ErrInvalidPageSize) {
		t.Fatalf("negative size: %v", err)
	}
	if _, err := f.listAccounts.Execute(c, dto.ListAccountsInput{PageToken: "garbage"}); !errors.Is(err, pagination.ErrInvalidToken) {
		t.Fatalf("bad token: %v", err)
	}
	empty, _ := pagination.EncodeToken(map[string]string{"id": ""})
	if _, err := f.listAccounts.Execute(c, dto.ListAccountsInput{PageToken: empty}); !errors.Is(err, account.ErrInvalidCursor) {
		t.Fatalf("empty cursor: %v", err)
	}
}

func TestAPIKeyLifecycleAndResolve(t *testing.T) {
	f := newFixture()
	c := context.Background()
	a, _ := f.createAccount.Execute(c, dto.CreateAccountInput{Name: "ci", Roles: []string{"admin"}})

	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{Name: "k"}); !errors.Is(err, apikey.ErrInvalidAccountID) {
		t.Fatalf("no account: %v", err)
	}
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: "nope", Name: "k"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: ""}); !errors.Is(err, apikey.ErrInvalidName) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: "k", ExpiresAt: f.clock.t}); !errors.Is(err, apikey.ErrInvalidExpiry) {
		t.Fatalf("past expiry: %v", err)
	}
	f.secrets.err = errors.New("entropy exhausted")
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: "k"}); err == nil || !strings.Contains(err.Error(), "generate api key secret") {
		t.Fatalf("secret failure: %v", err)
	}
	f.secrets.err = nil
	f.ids.err = errors.New("entropy exhausted")
	if _, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: "k"}); err == nil || !strings.Contains(err.Error(), "generate api key id") {
		t.Fatalf("id failure: %v", err)
	}
	f.ids.err = nil

	k1, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: "laptop"})
	if err != nil || k1.Key.AccountID != a.ID || k1.Key.Status != "ACTIVE" || k1.Key.Prefix != k1.Secret[:apikey.PrefixLen] || !k1.Key.ExpiresAt.IsZero() {
		t.Fatalf("create key = %+v, %v", k1, err)
	}
	f.clock.t = f.clock.t.Add(time.Second)
	exp := f.clock.t.Add(time.Hour)
	k2, err := f.createKey.Execute(c, dto.CreateAPIKeyInput{AccountID: a.ID, Name: "ci", ExpiresAt: exp})
	if err != nil || !k2.Key.ExpiresAt.Equal(exp) {
		t.Fatalf("create expiring key = %+v, %v", k2, err)
	}

	list, err := f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: a.ID})
	if err != nil || len(list.Keys) != 2 || list.Keys[0].ID != k2.Key.ID || list.Keys[1].ID != k1.Key.ID {
		t.Fatalf("list = %+v, %v (want newest first)", list, err)
	}
	if _, err := f.listKeys.Execute(c, dto.ListAPIKeysInput{}); !errors.Is(err, apikey.ErrInvalidAccountID) {
		t.Fatalf("list empty: %v", err)
	}
	if _, err := f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: "nope"}); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("list unknown: %v", err)
	}

	// Resolve: the happy path carries the account, not the key's name, and
	// the key's expiry (zero for k1) so a cache can honour it.
	p, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: k1.Secret})
	if err != nil || p.AccountID != a.ID || p.Name != "ci" || !slices.Equal(p.Roles, []string{"admin"}) || p.KeyID != k1.Key.ID || !p.ExpiresAt.IsZero() {
		t.Fatalf("resolve = %+v, %v", p, err)
	}
	if p2, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: k2.Secret}); err != nil || !p2.ExpiresAt.Equal(exp) {
		t.Fatalf("resolve expiring = %+v, %v", p2, err)
	}
	for name, tc := range map[string]struct {
		secret string
		want   error
	}{
		"short":   {"xk_short", apikey.ErrInvalidSecret},
		"unknown": {"xk_" + strings.Repeat("z", 43), apikey.ErrNotFound},
	} {
		if _, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: tc.secret}); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Expired: k2 past its expiry.
	f.clock.t = exp
	if _, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: k2.Secret}); !errors.Is(err, apikey.ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Disabled account: the key is fine, the account is not.
	if _, err := f.disableAccount.Execute(c, dto.DisableAccountInput{ID: a.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: k1.Secret}); !errors.Is(err, account.ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := f.enableAccount.Execute(c, dto.EnableAccountInput{ID: a.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: k1.Secret}); err != nil {
		t.Fatalf("re-enabled: %v", err)
	}

	// Revoke.
	rv, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: k1.Key.ID})
	if err != nil || rv.Status != "REVOKED" || !rv.RevokedAt.Equal(f.clock.t) {
		t.Fatalf("revoke = %+v, %v", rv, err)
	}
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: k1.Key.ID}); !errors.Is(err, apikey.ErrAlreadyRevoked) {
		t.Fatalf("revoke twice: %v", err)
	}
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{}); !errors.Is(err, apikey.ErrInvalidID) {
		t.Fatalf("revoke empty: %v", err)
	}
	if _, err := f.revokeKey.Execute(c, dto.RevokeAPIKeyInput{ID: "nope"}); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}
	if _, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: k1.Secret}); !errors.Is(err, apikey.ErrRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	// The account whose key resolves was deleted underneath (memory-only
	// scenario, the FK forbids it on Postgres): not found surfaces.
	list, _ = f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: a.ID})
	if len(list.Keys) != 2 || list.Keys[1].Status != "REVOKED" {
		t.Fatalf("list after revoke = %+v", list)
	}
}

func TestWhoAmI(t *testing.T) {
	uc := usecase.NewWhoAmI()
	out, err := uc.Execute(context.Background(), dto.WhoAmIInput{})
	if err != nil || !out.Anonymous || out.Subject != "" {
		t.Fatalf("anonymous = %+v, %v", out, err)
	}
	c := contextx.WithPrincipal(context.Background(), contextx.Principal{Subject: "acc-1", Name: "ops", Roles: []string{"admin"}, Method: "api_key", CredentialID: "key-1"})
	out, err = uc.Execute(c, dto.WhoAmIInput{})
	if err != nil || out.Anonymous || out.Subject != "acc-1" || out.Name != "ops" || !slices.Equal(out.Roles, []string{"admin"}) || out.Method != "api_key" || out.CredentialID != "key-1" {
		t.Fatalf("principal = %+v, %v", out, err)
	}
}

func TestBootstrap(t *testing.T) {
	f := newFixture()
	c := context.Background()
	boot := usecase.NewBootstrap(f.accounts, f.keys, f.ids, f.secrets, f.clock)
	exp := f.clock.t.Add(24 * time.Hour)

	// Validation happens before anything is written: a bad role leaves the store empty.
	if _, err := boot.Execute(c, dto.BootstrapInput{Name: "root", Roles: []string{"admin", " "}, KeyName: "k"}); !errors.Is(err, account.ErrInvalidRole) {
		t.Fatalf("bad role: %v", err)
	}
	if _, err := boot.Execute(c, dto.BootstrapInput{Name: "root", Roles: []string{"admin"}, KeyName: ""}); !errors.Is(err, apikey.ErrInvalidName) {
		t.Fatalf("bad key name: %v", err)
	}
	if got, _, _ := f.accounts.List(c, account.ListParams{Limit: 10}); len(got) != 0 {
		t.Fatalf("failed bootstrap wrote %d account(s)", len(got))
	}

	res, err := boot.Execute(c, dto.BootstrapInput{Name: "root", Roles: []string{"admin"}, KeyName: "bootstrap", ExpiresAt: exp})
	if err != nil || !res.Created || res.Account.Name != "root" || !slices.Equal(res.Account.Roles, []string{"admin"}) ||
		res.Key.AccountID != res.Account.ID || res.Key.Name != "bootstrap" || !res.Key.ExpiresAt.Equal(exp) ||
		!strings.HasPrefix(res.Secret, "xk_") || res.Key.Prefix != res.Secret[:8] {
		t.Fatalf("bootstrap = %+v, %v", res, err)
	}
	// The key resolves to the account it was made for.
	p, err := f.resolve.Execute(c, dto.ResolveAPIKeyInput{Secret: res.Secret})
	if err != nil || p.AccountID != res.Account.ID || p.KeyID != res.Key.ID || !slices.Equal(p.Roles, []string{"admin"}) {
		t.Fatalf("resolve bootstrap key = %+v, %v", p, err)
	}

	// Second run: an account exists → nothing written, Created false, no secret.
	again, err := boot.Execute(c, dto.BootstrapInput{Name: "other", Roles: []string{"admin"}, KeyName: "k"})
	if err != nil || again.Created || again.Secret != "" || again.Account.ID != "" {
		t.Fatalf("second bootstrap = %+v, %v", again, err)
	}
	if got, _, _ := f.accounts.List(c, account.ListParams{Limit: 10}); len(got) != 1 {
		t.Fatalf("second bootstrap changed the store: %d account(s)", len(got))
	}
	keys, err := f.listKeys.Execute(c, dto.ListAPIKeysInput{AccountID: res.Account.ID})
	if err != nil || len(keys.Keys) != 1 {
		t.Fatalf("keys after second bootstrap = %+v, %v", keys, err)
	}

	// Generator failures surface with their step.
	g := newFixture()
	g.ids.err = errors.New("entropy exhausted")
	if _, err := usecase.NewBootstrap(g.accounts, g.keys, g.ids, g.secrets, g.clock).Execute(c, dto.BootstrapInput{Name: "root", KeyName: "k"}); err == nil || !strings.Contains(err.Error(), "generate account id") {
		t.Fatalf("id failure: %v", err)
	}
	g.ids.err = nil
	g.secrets.err = errors.New("entropy exhausted")
	if _, err := usecase.NewBootstrap(g.accounts, g.keys, g.ids, g.secrets, g.clock).Execute(c, dto.BootstrapInput{Name: "root", KeyName: "k"}); err == nil || !strings.Contains(err.Error(), "generate api key secret") {
		t.Fatalf("secret failure: %v", err)
	}
}
