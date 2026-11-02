package postgres

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/infra/provider/mock"
)

var _ mock.Store = (*MockStore)(nil)

// MockStore keeps the mock provider's state in payment.mock_checkouts / payment.mock_refunds, so the
// mock of `serve`, of `consume` and of `expire` are one provider. Every method is a single SQL
// statement (Refund: one insert and, only when it inserted nothing, one read of a row that is never
// deleted), so a race on one ref is decided by the database, not by a lock inside a process.
type MockStore struct {
	db *gorm.DB
}

func NewMockStore(db *gorm.DB) *MockStore { return &MockStore{db: db} }

type mockCheckoutRow struct {
	Ref          string
	PaymentID    string
	Amount       int64
	Currency     string
	URL          string
	ExpiresAt    time.Time
	Paid         bool
	PaidAmount   *int64
	PaidCurrency *string
	Cancelled    bool
}

func (mockCheckoutRow) TableName() string { return Schema + ".mock_checkouts" }

func (s *MockStore) Create(c context.Context, ref string, co mock.Checkout) error {
	row := mockCheckoutRow{Ref: ref, PaymentID: co.PaymentID, Amount: co.Amount.Amount, Currency: co.Amount.Currency, URL: co.URL, ExpiresAt: co.ExpiresAt}
	if err := s.db.WithContext(c).Create(&row).Error; err != nil {
		return fmt.Errorf("create mock checkout: %w", err)
	}

	return nil
}

func (s *MockStore) Get(c context.Context, ref string) (mock.Checkout, bool, error) {
	var rows []mockCheckoutRow
	if err := s.db.WithContext(c).Where("ref = ?", ref).Limit(1).Find(&rows).Error; err != nil {
		return mock.Checkout{}, false, fmt.Errorf("read mock checkout: %w", err)
	}
	if len(rows) == 0 {
		return mock.Checkout{}, false, nil
	}
	r := rows[0]
	co := mock.Checkout{
		PaymentID: r.PaymentID, Amount: dto.MoneyOutput{Amount: r.Amount, Currency: r.Currency}, URL: r.URL,
		ExpiresAt: r.ExpiresAt.UTC(), Paid: r.Paid, Cancelled: r.Cancelled,
	}
	if r.Paid && r.PaidAmount != nil && r.PaidCurrency != nil {
		co.PaidWith = dto.MoneyOutput{Amount: *r.PaidAmount, Currency: *r.PaidCurrency}
	}

	return co, true, nil
}

func (s *MockStore) MarkPaid(c context.Context, ref string, with dto.MoneyOutput) (bool, error) {
	res := s.db.WithContext(c).Exec(`UPDATE `+Schema+`.mock_checkouts SET paid = true, paid_amount = ?, paid_currency = ? WHERE ref = ?`, with.Amount, with.Currency, ref)
	if res.Error != nil {
		return false, fmt.Errorf("pay mock checkout: %w", res.Error)
	}

	return res.RowsAffected > 0, nil
}

func (s *MockStore) MarkCancelled(c context.Context, ref string) (bool, error) {
	res := s.db.WithContext(c).Exec(`UPDATE `+Schema+`.mock_checkouts SET cancelled = true WHERE ref = ?`, ref)
	if res.Error != nil {
		return false, fmt.Errorf("cancel mock checkout: %w", res.Error)
	}

	return res.RowsAffected > 0, nil
}

// Refund inserts the key only for a PAID checkout; a key that is already there inserts nothing and is
// still a success (the provider's idempotency), which the follow-up read tells apart from "not paid".
func (s *MockStore) Refund(c context.Context, ref, key string) (bool, error) {
	res := s.db.WithContext(c).Exec(`INSERT INTO `+Schema+`.mock_refunds (ref, idempotency_key)
SELECT ref, ? FROM `+Schema+`.mock_checkouts WHERE ref = ? AND paid
ON CONFLICT (ref, idempotency_key) DO NOTHING`, key, ref)
	if res.Error != nil {
		return false, fmt.Errorf("refund mock checkout: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	var n int64
	if err := s.db.WithContext(c).Raw(`SELECT count(*) FROM `+Schema+`.mock_refunds WHERE ref = ? AND idempotency_key = ?`, ref, key).Scan(&n).Error; err != nil {
		return false, fmt.Errorf("read mock refund: %w", err)
	}

	return n > 0, nil
}

func (s *MockStore) Refunds(c context.Context, ref string) (int, error) {
	var n int
	if err := s.db.WithContext(c).Raw(`SELECT count(*) FROM `+Schema+`.mock_refunds WHERE ref = ?`, ref).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("count mock refunds: %w", err)
	}

	return n, nil
}

func (s *MockStore) Refs(c context.Context) ([]string, error) {
	var refs []string
	if err := s.db.WithContext(c).Raw(`SELECT ref FROM ` + Schema + `.mock_checkouts ORDER BY seq`).Scan(&refs).Error; err != nil {
		return nil, fmt.Errorf("list mock checkouts: %w", err)
	}

	return refs, nil
}
