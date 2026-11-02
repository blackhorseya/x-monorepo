package client

import (
	"github.com/google/wire"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
)

// ProviderSet is payment as another process's service, for the composition root that does not own the BC
// (apps/rpc): the client for payment-rpc, the paymentv1connect.PaymentServiceHandler it implements, and the
// PublicService that serves payment.v1.PublicPaymentService on the caller's own port. Needs config.Payment,
// config.Ordering, config.Auth, the otel providers and an OrderReader; builds nothing of the BC itself.
var ProviderSet = wire.NewSet(
	New,
	wire.Bind(new(paymentv1connect.PaymentServiceHandler), new(*Client)),
	NewPublicService,
)
