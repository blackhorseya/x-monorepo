package kernel

var (
	ErrInvalidAmount    = NewError(KindInvalidArgument, "money.invalid_amount", "amount must not be negative")
	ErrInvalidCurrency  = NewError(KindInvalidArgument, "money.invalid_currency", "currency must be a 3-letter ISO 4217 code")
	ErrCurrencyMismatch = NewError(KindInvalidArgument, "money.currency_mismatch", "currency mismatch")
)

// Money is an immutable value object stored in minor units.
type Money struct {
	amount   int64
	currency string
}

// NewMoney validates and creates Money.
func NewMoney(amount int64, currency string) (Money, error) {
	if amount < 0 {
		return Money{}, ErrInvalidAmount
	}
	if !isCurrencyCode(currency) {
		return Money{}, ErrInvalidCurrency
	}

	return Money{amount: amount, currency: currency}, nil
}

// ZeroMoney returns 0 in the given currency.
func ZeroMoney(currency string) Money { return Money{currency: currency} }

func (x Money) Amount() int64    { return x.amount }
func (x Money) Currency() string { return x.currency }

// Add returns x + o. Both must share a currency.
func (x Money) Add(o Money) (Money, error) {
	if x.currency != o.currency {
		return Money{}, ErrCurrencyMismatch
	}

	return Money{amount: x.amount + o.amount, currency: x.currency}, nil
}

// Multiply returns x * n.
func (x Money) Multiply(n int64) Money {
	return Money{amount: x.amount * n, currency: x.currency}
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}

	return true
}
