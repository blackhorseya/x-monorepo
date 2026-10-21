package postgres

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// Inbox is usecasex.Inbox on a bounded context's `<schema>.inbox` table (one
// row per consumed message id). Record inserts through the transaction in
// the context (TxOrDB), so with usecasex.Transactional outside it the row
// commits with the use case's writes and rolls back with its failure; a
// concurrent redelivery blocks on the primary key until the first attempt
// commits, then sees the conflict.
type Inbox struct {
	db    *gorm.DB
	table string
}

// NewInbox returns the inbox of schema (the BC's Schema constant — never
// user input, it is interpolated into SQL). The table ships in the BC's
// migrations: inbox(message_id VARCHAR(36) PRIMARY KEY, consumed_at).
func NewInbox(db *gorm.DB, schema string) *Inbox {
	return &Inbox{db: db, table: schema + ".inbox"}
}

// Record inserts id; first is false when the row already existed.
func (i *Inbox) Record(c context.Context, id string) (bool, error) {
	res := TxOrDB(c, i.db).Exec(`INSERT INTO `+i.table+` (message_id) VALUES (?) ON CONFLICT (message_id) DO NOTHING`, id)
	if res.Error != nil {
		return false, fmt.Errorf("insert %s: %w", i.table, res.Error)
	}

	return res.RowsAffected == 1, nil
}
