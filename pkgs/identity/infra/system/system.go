// Package system implements ports backed by the OS (randomness, wall clock).
package system

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var (
	_ port.IDGenerator     = (*UUIDGenerator)(nil)
	_ port.SecretGenerator = (*SecretGenerator)(nil)
	_ port.Clock           = (*Clock)(nil)
)

// UUIDGenerator generates RFC 4122 v4 UUIDs.
type UUIDGenerator struct{}

func NewUUIDGenerator() *UUIDGenerator { return &UUIDGenerator{} }

func (i *UUIDGenerator) NewID(c context.Context) (string, error) {
	ctx := contextx.FromContext(c)
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
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
