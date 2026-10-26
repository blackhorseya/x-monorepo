package main

import "github.com/blackhorseya/x-monorepo/pkgs/platform/server"

// NewCredentialResolver: no bounded context manages credentials yet, so the
// auth interceptor only knows the config's static keys and jwt.
func NewCredentialResolver() server.CredentialResolver { return nil }
