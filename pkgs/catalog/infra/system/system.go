// Package system implements ports backed by the OS (wall clock).
package system

import (
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/catalog/application/port"
)

var _ port.Clock = (*Clock)(nil)

// Clock returns UTC wall-clock time.
type Clock struct{}

func NewClock() *Clock { return &Clock{} }

func (i *Clock) Now() time.Time { return time.Now().UTC() }
