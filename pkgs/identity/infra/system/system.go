// Package system implements ports backed by the OS (randomness, wall clock).
package system

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
)

var (
	_ port.IDGenerator     = (*UUIDGenerator)(nil)
	_ port.SecretGenerator = (*SecretGenerator)(nil)
	_ port.Clock           = (*Clock)(nil)
)

// UUIDGenerator generates UUID v7 ids (see idgen: opaque, never a credential,
// never an ordering on their own).
type UUIDGenerator struct{}

func NewUUIDGenerator() *UUIDGenerator { return &UUIDGenerator{} }

func (i *UUIDGenerator) NewID(c context.Context) (string, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return "", err
	}

	return idgen.New()
}

// SecretPrefix marks a secret as one of ours in a client's config or a leak scan.
const SecretPrefix = "xk_"

// SecretGenerator mints API key secrets: SecretPrefix + 32 random bytes as
// base64url (43 characters, no padding) — 256 bits, so the hash alone is a
// safe store and a guess never lands.
type SecretGenerator struct{}

func NewSecretGenerator() *SecretGenerator { return &SecretGenerator{} }

func (i *SecretGenerator) NewSecret(c context.Context) (string, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return SecretPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Clock returns UTC wall-clock time.
type Clock struct{}

func NewClock() *Clock { return &Clock{} }

func (i *Clock) Now() time.Time { return time.Now().UTC() }
