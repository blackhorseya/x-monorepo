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
// already exists) is not an error: the command is safe to leave in a job
// that runs on every rollout.
func (h *Bootstrap) Run(c context.Context, out io.Writer, name string, roles []string, keyName string, expiresAt time.Time) error {
	res, err := h.bootstrap.Execute(c, dto.BootstrapInput{Name: name, Roles: roles, KeyName: keyName, ExpiresAt: expiresAt})
	if err != nil {
		return err
	}
	if !res.Created {
		_, _ = fmt.Fprintln(out, "already bootstrapped: an account exists, nothing written")
		return nil
	}
	expires := "never"
	if !res.Key.ExpiresAt.IsZero() {
		expires = res.Key.ExpiresAt.UTC().Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(out, "account %s (%s) roles=%v\napi key %s (%s) prefix=%s expires=%s\n",
		res.Account.ID, res.Account.Name, res.Account.Roles, res.Key.ID, res.Key.Name, res.Key.Prefix, expires)
	_, _ = fmt.Fprintf(out, "X-API-Key: %s\nThis secret is shown once; only its hash is stored.\n", res.Secret)

	return nil
}
