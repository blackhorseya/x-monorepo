// Package rpc adapts the generated payment.v1 contract to use cases. SPEC STAGE: the handlers answer
// with an empty response.
package rpc

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ paymentv1connect.PaymentServiceHandler = (*PaymentService)(nil)

// Use-case aliases keep handler signatures readable.
type (
	StartPaymentUC = usecasex.UseCase[dto.StartPaymentInput, *dto.PaymentOutput]
	GetPaymentUC   = usecasex.UseCase[dto.GetPaymentInput, *dto.PaymentOutput]
)

// ProviderUnavailableCode is the x-error-code of StartPayment failing because the provider did not
// give a usable answer.
const ProviderUnavailableCode = "payment.provider_unavailable"

// PaymentService implements the generated PaymentServiceHandler.
type PaymentService struct {
	startPayment StartPaymentUC
	getPayment   GetPaymentUC
}

func NewPaymentService(startPayment StartPaymentUC, getPayment GetPaymentUC) *PaymentService {
	return &PaymentService{startPayment: startPayment, getPayment: getPayment}
}

// Route returns the mount path and handler.
func (h *PaymentService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return paymentv1connect.NewPaymentServiceHandler(h, opts...)
}

func (h *PaymentService) StartPayment(context.Context, *connect.Request[paymentv1.StartPaymentRequest]) (*connect.Response[paymentv1.StartPaymentResponse], error) {
	return connect.NewResponse(&paymentv1.StartPaymentResponse{Payment: &paymentv1.Payment{}}), nil
}

func (h *PaymentService) GetPayment(context.Context, *connect.Request[paymentv1.GetPaymentRequest]) (*connect.Response[paymentv1.GetPaymentResponse], error) {
	return connect.NewResponse(&paymentv1.GetPaymentResponse{Payment: &paymentv1.Payment{}}), nil
}
