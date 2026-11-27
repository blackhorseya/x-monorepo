// Package acceptance is the contract between the BDD runner (apps/rpc's TestAcceptance) and the
// step definitions of a bounded context (pkgs/<bc>/acceptance, added with that BC).
//
// Only test files may import it: it pulls in godog, which no shipped binary links.
package acceptance

import (
	"errors"
	"fmt"

	"github.com/cucumber/godog"
)

// Env is what one scenario runs against: its own database and the rpc the runner started in
// the test process. The runner creates a fresh Env per scenario; the steps read it.
type Env struct {
	// RPCURL is the base URL of the in-process rpc server (auth.mode=required), empty until
	// the scenario's setup step has started it.
	RPCURL string
	// DatabaseURL is this scenario's own database, copied from the migrated template.
	DatabaseURL string
	// AdminKey is an API key with the admin role, accepted by the rpc in RPCURL.
	AdminKey string
	// PermissionKeys holds, for every permission the rpc has registered ("<bc>.<function>", platform.admin
	// included), an API key whose only role is the one that permission stands for.
	PermissionKeys map[string]string
	// NoRoleKey is an API key with no role at all: an authenticated caller who holds no permission.
	NoRoleKey string

	closers []func() error
}

// KeyFor is the API key that holds permission and nothing else; an error for a permission the rpc did not register.
func (e *Env) KeyFor(permission string) (string, error) {
	key, ok := e.PermissionKeys[permission]
	if !ok {
		return "", fmt.Errorf("acceptance: no key for permission %q: the rpc registered no such permission", permission)
	}

	return key, nil
}

// Defer registers something a step started (a server, a pool) to be closed when the scenario ends.
// The runner calls Close before it drops the scenario's database, so whatever holds a connection to
// that database belongs here, not in a godog After hook (those run in registration order, which a step
// file cannot see).
func (e *Env) Defer(closer func() error) { e.closers = append(e.closers, closer) }

// Close runs what Defer registered, last registered first, and returns every error. The runner calls it.
func (e *Env) Close() error {
	var errs []error
	for i := len(e.closers) - 1; i >= 0; i-- {
		errs = append(errs, e.closers[i]())
	}
	e.closers = nil
	return errors.Join(errs...)
}

// Register adds the steps (and hooks) of one feature area to a scenario's context.
// The runner calls every Register once per scenario, with that scenario's Env.
type Register func(sc *godog.ScenarioContext, env *Env)
