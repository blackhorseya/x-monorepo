// Package rpc adapts the generated payment.v1 contract to use cases.
package rpc

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

var _ paymentv1connect.PaymentServiceHandler = (*PaymentService)(nil)

// Use-case aliases keep handler signatures readable.
type (
	StartPaymentUC = usecasex.UseCase[dto.StartPaymentInput, *dto.PaymentOutput]
	GetPaymentUC   = usecasex.UseCase[dto.GetPaymentInput, *dto.PaymentOutput]
)

// ProviderUnavailableCode is the x-error-code of StartPayment failing because the provider did not
// give a usable answer: nothing was changed and the same request can be retried.
const ProviderUnavailableCode = "payment.provider_unavailable"

// providerUnavailable is UNAVAILABLE with a fixed message: whatever the provider said is neither
// logged nor echoed (it is untrusted and may reflect a credential, #347).
func providerUnavailable() error {
	ce := connect.NewError(connect.CodeUnavailable, errors.New("the payment provider could not be reached, retry the request"))
	ce.Meta().Set("x-error-code", ProviderUnavailableCode)

	return ce
}

// PaymentService implements the generated PaymentServiceHandler. It does no role check of its own:
// the only way in is the auth interceptor holding the service key, and customer_id / admin are
// that caller's claims (see the service's doc in service.proto).
type PaymentService struct {
	startPayment StartPaymentUC
	getPayment   GetPaymentUC
}

func NewPaymentService(startPayment StartPaymentUC, getPayment GetPaymentUC) *PaymentService {
	return &PaymentService{startPayment: startPayment, getPayment: getPayment}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (h *PaymentService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return paymentv1connect.NewPaymentServiceHandler(h, opts...)
}

func (h *PaymentService) StartPayment(
	c context.Context,
	req *connect.Request[paymentv1.StartPaymentRequest],
) (*connect.Response[paymentv1.StartPaymentResponse], error) {
	out, err := h.startPayment.Execute(c, dto.StartPaymentInput{
		OrderID: req.Msg.GetOrderId(), CustomerID: req.Msg.GetCustomerId(), Admin: req.Msg.GetAdmin(),
	})
	if err != nil {
		if errors.Is(err, port.ErrProviderUnavailable) {
			return nil, providerUnavailable()
		}

		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&paymentv1.StartPaymentResponse{Payment: toProtoPayment(out)}), nil
}

func (h *PaymentService) GetPayment(
	c context.Context,
	req *connect.Request[paymentv1.GetPaymentRequest],
) (*connect.Response[paymentv1.GetPaymentResponse], error) {
	out, err := h.getPayment.Execute(c, dto.GetPaymentInput{
		OrderID: req.Msg.GetOrderId(), CustomerID: req.Msg.GetCustomerId(), Admin: req.Msg.GetAdmin(),
	})
	if err != nil {
		return nil, server.ToConnectError(err)
	}

	return connect.NewResponse(&paymentv1.GetPaymentResponse{Payment: toProtoPayment(out)}), nil
}

var statusToProto = map[string]paymentv1.PaymentStatus{
	"PENDING":      paymentv1.PaymentStatus_PAYMENT_STATUS_PENDING,
	"CAPTURED":     paymentv1.PaymentStatus_PAYMENT_STATUS_CAPTURED,
	"EXPIRED":      paymentv1.PaymentStatus_PAYMENT_STATUS_EXPIRED,
	"CANCELLED":    paymentv1.PaymentStatus_PAYMENT_STATUS_CANCELLED,
	"REFUNDING":    paymentv1.PaymentStatus_PAYMENT_STATUS_REFUNDING,
	"REFUNDED":     paymentv1.PaymentStatus_PAYMENT_STATUS_REFUNDED,
	"NEEDS_REVIEW": paymentv1.PaymentStatus_PAYMENT_STATUS_NEEDS_REVIEW,
}

func toProtoPayment(p *dto.PaymentOutput) *paymentv1.Payment {
	out := &paymentv1.Payment{
		Id: p.ID, OrderId: p.OrderID, CustomerId: p.CustomerID,
		Amount:      &commonv1.Money{AmountMinor: p.Amount.Amount, Currency: p.Amount.Currency},
		Status:      statusToProto[p.Status], // an unknown status stays UNSPECIFIED
		ExpiresAt:   timestamppb.New(p.ExpiresAt),
		CheckoutUrl: p.CheckoutURL,
		UpdateTime:  timestamppb.New(p.UpdateTime),
	}
	if !p.CheckoutExpiresAt.IsZero() {
		out.CheckoutExpiresAt = timestamppb.New(p.CheckoutExpiresAt)
	}

	return out
}
