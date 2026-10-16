package commonv1

import "github.com/blackhorseya/x-monorepo/pkgs/kernel"

// Hand-written helpers live beside the generated code (buf.gen.yaml has no
// clean: true). They are the only place kernel.Money meets its wire form.

// FromMoney converts a kernel Money value object to its wire form.
func FromMoney(m kernel.Money) *Money {
	return &Money{AmountMinor: m.Amount(), Currency: m.Currency()}
}

// ToMoney validates and converts a wire Money to the value object. nil → zero USD-less Money error.
func ToMoney(m *Money) (kernel.Money, error) {
	return kernel.NewMoney(m.GetAmountMinor(), m.GetCurrency())
}
