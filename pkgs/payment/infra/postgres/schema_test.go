package postgres_test

import (
	"testing"
)

const insertSQL = `INSERT INTO payment.payments
	(id, order_id, customer_id, amount, currency, status, order_outcome, expires_at, version, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, now(), 1, now())`

// The schema refuses what the aggregate refuses, or a hand-written row would make GetPayment fail
// on a request that is fine (the same combinations Reconstitute refuses).
func TestSchema_AgreesWithTheDomain(t *testing.T) {
	db := testDB(t)

	if err := db.Exec(insertSQL, "ok-1", "ord-ok-1", "cust", 10, "TWD", "PENDING", "NONE").Error; err != nil {
		t.Fatalf("a valid row: %v", err)
	}

	refused := map[string][]any{
		"unknown status":                 {"x1", "o-x1", "cust", 10, "TWD", "PAID", "NONE"},
		"unknown outcome":                {"x2", "o-x2", "cust", 10, "TWD", "PENDING", "GONE"},
		"negative amount":                {"x3", "o-x3", "cust", -1, "TWD", "PENDING", "NONE"},
		"currency not 3 capital letters": {"x4", "o-x4", "cust", 10, "twd", "PENDING", "NONE"},
		"empty id":                       {"", "o-x5", "cust", 10, "TWD", "PENDING", "NONE"},
		"empty order":                    {"x6", "", "cust", 10, "TWD", "PENDING", "NONE"},
		"empty customer":                 {"x7", "o-x7", "", 10, "TWD", "PENDING", "NONE"},
		"pending that heard the end":     {"x8", "o-x8", "cust", 10, "TWD", "PENDING", "CANCELLED"},
		"captured that heard the end":    {"x9", "o-x9", "cust", 10, "TWD", "CAPTURED", "COMPLETED_UNPAID"},
		"cancelled without an end":       {"y1", "o-y1", "cust", 10, "TWD", "CANCELLED", "NONE"},
		"refunding without an end":       {"y2", "o-y2", "cust", 10, "TWD", "REFUNDING", "NONE"},
		"refunded without an end":        {"y3", "o-y3", "cust", 10, "TWD", "REFUNDED", "NONE"},
		"second payment for an order":    {"y4", "ord-ok-1", "cust", 10, "TWD", "PENDING", "NONE"},
		"reused payment id":              {"ok-1", "o-y5", "cust", 10, "TWD", "PENDING", "NONE"},
	}
	for name, args := range refused {
		if err := db.Exec(insertSQL, args...).Error; err == nil {
			t.Errorf("%s: the schema accepted the row", name)
		}
	}

	accepted := map[string][]any{
		"expired, any outcome":      {"a1", "o-a1", "cust", 10, "TWD", "EXPIRED", "COMPLETED_UNPAID"},
		"expired, no end yet":       {"a2", "o-a2", "cust", 10, "TWD", "EXPIRED", "NONE"},
		"needs review, any outcome": {"a3", "o-a3", "cust", 10, "TWD", "NEEDS_REVIEW", "CANCELLED"},
		"refunding after cancel":    {"a4", "o-a4", "cust", 10, "TWD", "REFUNDING", "CANCELLED"},
		"zero amount":               {"a5", "o-a5", "cust", 0, "TWD", "PENDING", "NONE"},
	}
	for name, args := range accepted {
		if err := db.Exec(insertSQL, args...).Error; err != nil {
			t.Errorf("%s: the schema refused a row the domain accepts: %v", name, err)
		}
	}
}
