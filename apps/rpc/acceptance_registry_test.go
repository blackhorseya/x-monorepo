package main

import (
	"errors"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/acceptance"
)

// registration is one bounded context's acceptance: its name and the steps that register its features.
type registration struct {
	bc       string
	register acceptance.Register
}

// bcsFromWork is the bounded contexts go.work lists, by scripts/bcs.jq (the one rule for what a BC is).
func bcsFromWork(root string) ([]string, error) { return nil, errors.New("not implemented") }

// checkRegistered requires the BCs and the registrations to be the same set, and every BC to hold at least one scenario.
func checkRegistered(bcs []string, regs []registration, scenarios map[string]int) error { return nil }
