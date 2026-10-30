package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/identity/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ port.AuditLog = (*AuditLog)(nil)

// AuditLog keeps the events in memory, for local dev and tests.
type AuditLog struct {
	mu     sync.Mutex
	events []dto.AuditEvent
}

func NewAuditLog() *AuditLog { return &AuditLog{} }

func (i *AuditLog) Record(c context.Context, e dto.AuditEvent) error {
	if err := contextx.FromContext(c).Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	i.events = append(i.events, e)

	return nil
}

// Events returns what was recorded, oldest first.
func (i *AuditLog) Events() []dto.AuditEvent {
	i.mu.Lock()
	defer i.mu.Unlock()

	return slices.Clone(i.events)
}
