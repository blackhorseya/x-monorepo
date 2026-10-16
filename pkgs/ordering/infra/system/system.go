// Package system implements ports backed by the OS (randomness, wall clock).
package system

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var (
	_ port.IDGenerator = (*UUIDGenerator)(nil)
	_ port.Clock       = (*Clock)(nil)
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

// Clock returns UTC wall-clock time.
type Clock struct{}

func NewClock() *Clock { return &Clock{} }

func (i *Clock) Now() time.Time { return time.Now().UTC() }
