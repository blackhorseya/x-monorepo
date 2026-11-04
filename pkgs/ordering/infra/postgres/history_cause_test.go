package postgres_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Migration 0011 widens the cause CHECK of ordering.order_history to the two causes inventory's
// typed request adds, and nothing else: every cause is accepted, a value outside the table is still
// refused (the control that the CHECK is there at all).
func TestMigration0011_CauseCheckAcceptsTheInventoryCauses(t *testing.T) {
	c, db := historyDB(t)
	insert := func(orderID, cause string) error {
		return db.WithContext(c).Exec(
			`INSERT INTO ordering.order_history (order_id, seq, event, from_status, to_status, occurred_at, actor_kind, actor_subject, cause)
			 VALUES (?, 1, 'CANCELLED', 'CONFIRMED', 'CANCELLED', ?, 'SYSTEM', '', ?)`, orderID, time.Now(), cause).Error
	}

	causes := []string{"UNSPECIFIED", "RPC_REQUEST", "PAYMENT_CAPTURED", "PAYMENT_EXPIRED", "STOCK_RESERVED", "CANCELLATION_REQUESTED", "STOCK_SHORTAGE", "RESERVATION_EXPIRED"}
	for i, cause := range causes {
		if err := insert(fmt.Sprintf("o-cause-%d", i), cause); err != nil {
			t.Errorf("cause %s refused: %v", cause, err)
		}
	}

	err := insert("o-cause-bogus", "SOMETHING_ELSE")
	if err == nil || !strings.Contains(err.Error(), "order_history") {
		t.Errorf("a cause outside the table was accepted or failed for another reason: %v", err)
	}
}
