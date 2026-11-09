package main

import (
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/archtest"
)

// A composition root reaches a BC only through its provider.go (and adapters):
// the import rules in pkgs/platform/archtest/imports.go. A new app needs this file.
func TestArchitecture(t *testing.T) { archtest.RunImports(t, ".") }
