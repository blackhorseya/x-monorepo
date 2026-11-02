package dto

import "time"

// What the application and a payment provider say to each other. Plain data like every dto; the
// PaymentProvider port (application/port) speaks in these.

// ProviderCheckoutInput asks the provider for a hosted checkout page.
type ProviderCheckoutInput struct {
	// PaymentID and OrderID identify the payment on the provider's side.
	PaymentID string
	OrderID   string
	Amount    MoneyOutput
	// ExpiresAt is when the checkout must stop being payable: the payment's own window end. One payment has
	// one checkout for its whole window (docs/payment.md T2), so the page may not end earlier (the buyer
	// could not pay for the rest of the window and could not reopen) nor outlive it. Zero = the provider's
	// own validity.
	ExpiresAt time.Time
}

// ProviderCheckoutOutput is the hosted page the provider opened. Card data is typed there; it never
// reaches us.
type ProviderCheckoutOutput struct {
	// Ref is the provider's reference for later queries, cancel and refund.
	Ref string
	URL string
	// ExpiresAt is when the page stops working.
	ExpiresAt time.Time
}

// ProviderPaymentOutput is the provider's answer to "what happened to this checkout".
type ProviderPaymentOutput struct {
	// Paid: money was collected. Amount says how much and in which currency; it is meaningful only
	// when Paid.
	Paid   bool
	Amount MoneyOutput
}

// ProviderRefundInput asks the provider to give a payment back in full.
type ProviderRefundInput struct {
	// IdempotencyKey is the payment id: asking again refunds nothing twice.
	IdempotencyKey string
	Ref            string
	Amount         MoneyOutput
}

// ProviderWebhookOutput is a verified provider notification.
type ProviderWebhookOutput struct {
	// ID is unique per provider, so a redelivery is recognised.
	ID string
	// Type is the provider's event type; only "payment.captured" moves a payment.
	Type   string
	Ref    string
	Amount MoneyOutput
}
