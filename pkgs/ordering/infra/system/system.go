// Package system implements ports backed by the OS (randomness, wall clock).
package system

import (
	"context"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
)

var (
	_ port.IDGenerator = (*UUIDGenerator)(nil)
	_ port.Clock       = (*Clock)(nil)
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

// Clock returns UTC wall-clock time.
type Clock struct{}

func NewClock() *Clock { return &Clock{} }

func (i *Clock) Now() time.Time { return time.Now().UTC() }
