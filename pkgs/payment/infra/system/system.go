// Package system implements ports backed by the OS (wall clock).
package system

import (
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/idgen"
)

var _ port.Clock = (*Clock)(nil)

// Clock returns UTC wall-clock time.
type Clock struct{}

func NewClock() *Clock { return &Clock{} }

func (i *Clock) Now() time.Time { return time.Now().UTC() }

var _ port.IDGenerator = (*IDGenerator)(nil)

// IDGenerator creates UUID v7 ids (platform/idgen, #365).
type IDGenerator struct{}

func NewIDGenerator() *IDGenerator { return &IDGenerator{} }

func (i *IDGenerator) NewID() (string, error) { return idgen.New() }
