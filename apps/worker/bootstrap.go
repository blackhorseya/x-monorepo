package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres"
)

// newBootstrapCmd is `worker bootstrap`: identity's first account and its
// first API key, on an empty store. The use case is identity's (adapters/cli,
// use case Bootstrap); this file is flags and wiring.
func newBootstrapCmd(load func() (*config.AppConfig, error)) *cobra.Command {
	var (
		name       string
		roles      []string
		keyName    string
		expiresIn  time.Duration
		adminEmail string

		expectTarget   string
		allowPlaintext bool
	)
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "create identity's first account and API key when no account exists",
		Long: `Create the first account (--name, --role; the role defaults to auth.admin_role so the
key can call IdentityService) and its first API key (--key-name, --expires-in) in one
transaction, and print the key's secret once. Only an empty store is touched: when any
account exists the command prints "already bootstrapped" and exits 0, so running it
again is a no-op — but two runs racing on an empty store can both create (the guard is
a read, not a constraint); run it from one place.

Run it only on the operator's own machine (kubectl port-forward to the database). Its
stdout carries the secret, so never run it inside the cluster — a Pod's stdout lands in
the Pod log. See docs/operations.md.

Before it connects, the command parses postgres.url the way the connection will and
refuses unless every attempt verifies the server (sslmode=verify-ca with sslrootcert, or
verify-full; disable, allow, prefer and require without sslrootcert are refused).
--expect-target host:port is required and every connection attempt must dial exactly it:
the host parsed from postgres.url can be a piece of a malformed password (an unencoded @),
so only the target you typed is trusted, and it is the only value an error will print.
--insecure-allow-plaintext skips the TLS check for a local development database only;
--expect-target still applies.

With the printed X-API-Key, CreateAccount / CreateApiKey make the real accounts; the
static auth.api_keys entry (if any) stays the break-glass path. The secret goes to
stdout only, never to the log.

With --admin-email <email> the OIDC path replaces the API key: the first account (--name,
--role) is created together with an invite for that email, valid 7 days and single use.
No API key is made and nothing secret exists to print — the deployer signs in at the
identity provider with that verified email and calls ClaimInvite. --key-name and
--expires-in belong to the API-key path and are refused with it. The same empty-store
guard, --expect-target and TLS rules apply. If nobody claims it in 7 days, reissue the
invite with the static auth.api_keys admin key (CreateInvite).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// First, before the config is read or the URL parsed: the host in the URL
			// may be a piece of a malformed password, so only a target the operator
			// typed can be trusted (and is the only value an error will ever print).
			if strings.TrimSpace(expectTarget) == "" {
				return errors.New("--expect-target is required: pass the host:port of the database you mean to reach (for example 127.0.0.1:15432 behind kubectl port-forward)")
			}
			if strings.TrimSpace(name) == "" {
				return errors.New("--name must not be empty")
			}
			if adminEmail != "" && (cmd.Flags().Changed("key-name") || cmd.Flags().Changed("expires-in")) {
				return errors.New("--key-name and --expires-in do not apply with --admin-email: it creates an invite, not an API key")
			}
			if strings.TrimSpace(keyName) == "" {
				return errors.New("--key-name must not be empty")
			}
			if expiresIn < 0 {
				return fmt.Errorf("--expires-in must not be negative (0 = never)")
			}
			if _, _, err := net.SplitHostPort(expectTarget); err != nil {
				return errors.New("--expect-target must be host:port")
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			if cfg.Postgres.URL == "" {
				return errors.New("postgres.url is required: accounts live in Postgres")
			}
			if err := postgres.CheckConn(cfg.Postgres.URL, postgres.ConnPolicy{ExpectTarget: expectTarget, AllowPlaintext: allowPlaintext}); err != nil {
				return fmt.Errorf("refusing to connect: %w", err)
			}
			if len(roles) == 0 {
				roles = []string{cfg.Auth.AdminRole}
			}
			var expiresAt time.Time
			if expiresIn > 0 {
				expiresAt = time.Now().Add(expiresIn)
			}
			boot, cleanup, err := InitializeBootstrap(cfg)
			if err != nil {
				return err
			}
			defer cleanup()

			if adminEmail != "" {
				return boot.RunInvite(cmd.Context(), cmd.OutOrStdout(), name, roles, adminEmail)
			}

			return boot.Run(cmd.Context(), cmd.OutOrStdout(), name, roles, keyName, expiresAt)
		},
	}
	cmd.Flags().StringVar(&name, "name", "root", "name of the first account")
	cmd.Flags().StringArrayVar(&roles, "role", nil, "role of the first account (repeatable; default: auth.admin_role)")
	cmd.Flags().StringVar(&keyName, "key-name", "bootstrap", "name of the first API key")
	cmd.Flags().DurationVar(&expiresIn, "expires-in", 0, "the key expires this long from now (Go duration, e.g. 720h; 0 = never)")
	cmd.Flags().StringVar(&adminEmail, "admin-email", "", "create the first admin account with an invite for this email instead of an API key (the deployer signs in at the IdP and calls ClaimInvite)")
	cmd.Flags().StringVar(&expectTarget, "expect-target", "", "required: the host:port every database connection must go to (e.g. 127.0.0.1:15432 behind kubectl port-forward); anything else is refused before connecting")
	cmd.Flags().BoolVar(&allowPlaintext, "insecure-allow-plaintext", false, "skip the TLS check, for a local development database only; --expect-target still applies")

	return cmd
}
