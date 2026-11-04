package memory

import (
	"context"
	"sync"

	"github.com/blackhorseya/x-monorepo/pkgs/ordering/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/ordering/domain/order"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ port.HistoryStore = (*HistoryStore)(nil)

// HistoryStore is the no-database port.HistoryStore (task run, the default E2E). There is no
// transaction to join: like the memory Transactor it has nothing to roll back.
type HistoryStore struct {
	mu   sync.Mutex
	rows map[order.ID][]order.HistoryEntry
}

func NewHistoryStore() *HistoryStore {
	return &HistoryStore{rows: make(map[order.ID][]order.HistoryEntry)}
}

// Append numbers each entry len+1 of its order's rows, under one lock, so numbering is contiguous
// however many callers there are.
func (i *HistoryStore) Append(c context.Context, entries ...order.HistoryEntry) error {
	if err := contextx.FromContext(c).Err(); err != nil {
		return err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	for _, e := range entries {
		seq := len(i.rows[e.OrderID()]) + 1
		stored, err := order.ReconstituteHistoryEntry(order.HistoryParams{
			OrderID: e.OrderID(), Seq: seq, From: e.From(), To: e.To(), OccurredAt: e.OccurredAt(),
			ActorKind: e.ActorKind(), ActorSubject: e.ActorSubject(), Cause: e.Cause(),
		})
		if err != nil {
			return err
		}
		i.rows[e.OrderID()] = append(i.rows[e.OrderID()], stored)
	}

	return nil
}

func (i *HistoryStore) List(c context.Context, id order.ID) ([]order.HistoryEntry, error) {
	if err := contextx.FromContext(c).Err(); err != nil {
		return nil, err
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	return append([]order.HistoryEntry{}, i.rows[id]...), nil
}
