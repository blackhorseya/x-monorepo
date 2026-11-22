// Package acceptance is the contract between the BDD runner (apps/rpc's TestAcceptance) and the
// step definitions of a bounded context (pkgs/<bc>/acceptance, added with that BC).
//
// Only test files may import it: it pulls in godog, which no shipped binary links.
package acceptance

import "github.com/cucumber/godog"

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
}

// Register adds the steps (and hooks) of one feature area to a scenario's context.
// The runner calls every Register once per scenario, with that scenario's Env.
type Register func(sc *godog.ScenarioContext, env *Env)
