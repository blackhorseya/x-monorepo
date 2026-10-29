// Package cli adapts the identity use cases an operator runs from the worker
// binary (`worker bootstrap`). Like adapters/rpc it holds decorated
// usecasex.UseCase values and knows nothing of cobra: flags are parsed in
// apps/worker, which hands this adapter plain values and a writer.
package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type BootstrapUC = usecasex.UseCase[dto.BootstrapInput, *dto.BootstrapOutput]

// Bootstrap creates the first account and its key on an empty store and
// prints the secret — once, to out, never to the log.
type Bootstrap struct {
	bootstrap BootstrapUC
}

func NewBootstrap(bootstrap BootstrapUC) *Bootstrap { return &Bootstrap{bootstrap: bootstrap} }

// Run bootstraps the store and reports on out. Created false (an account
// already exists) is not an error: running it again is a no-op.
//
// A failed write to out is an error: by then the transaction has committed,
// and a secret that never reached the operator is lost (only its hash is
// stored), so exiting 0 would hide it. The error names the account and key —
// never the secret — and points at the break-glass path.
func (h *Bootstrap) Run(c context.Context, out io.Writer, name string, roles []string, keyName string, expiresAt time.Time) error {
	res, err := h.bootstrap.Execute(c, dto.BootstrapInput{Name: name, Roles: roles, KeyName: keyName, ExpiresAt: expiresAt})
	if err != nil {
		return err
	}
	if !res.Created {
		if _, err := fmt.Fprintln(out, "already bootstrapped: an account exists, nothing written"); err != nil {
			return fmt.Errorf("write bootstrap result: %w", err)
		}

		return nil
	}
	expires := "never"
	if !res.Key.ExpiresAt.IsZero() {
		expires = res.Key.ExpiresAt.UTC().Format(time.RFC3339)
	}
	_, err = fmt.Fprintf(out, "account %s (%s) roles=%v\napi key %s (%s) prefix=%s expires=%s\nX-API-Key: %s\nThis secret is shown once; only its hash is stored.\n",
		res.Account.ID, res.Account.Name, res.Account.Roles, res.Key.ID, res.Key.Name, res.Key.Prefix, expires, res.Secret)
	if err != nil {
		return fmt.Errorf("account %s and api key %s (prefix %s) were created but the secret was not delivered; sign in with a config auth.api_keys (break-glass) key and create a new key: %w",
			res.Account.ID, res.Key.ID, res.Key.Prefix, err)
	}

	return nil
}

// RunInvite is Run for `worker bootstrap --admin-email` (RED stub).
func (h *Bootstrap) RunInvite(c context.Context, out io.Writer, name string, roles []string, adminEmail string) error {
	return fmt.Errorf("not implemented")
}
