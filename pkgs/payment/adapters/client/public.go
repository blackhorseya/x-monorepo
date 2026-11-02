package client

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	orderingv1 "github.com/blackhorseya/x-monorepo/pkgs/ordering/contract/v1"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/admin"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
)

var _ paymentv1connect.PublicPaymentServiceHandler = (*PublicService)(nil)

const (
	// DisabledCode is the x-error-code of StartPayment while payment is off for this order (docs/payment.md §6).
	DisabledCode = "payment.disabled"
	// unauthenticatedCode is the x-error-code the auth interceptor uses for "no credential".
	unauthenticatedCode = "auth.unauthenticated"
)

// OrderReader is the one question the entry asks ordering: the order as the caller may see it. apps/rpc
// binds it to ordering's own handler in the same process, called with the caller's context, so ordering's
// ownership applies (someone else's order is NOT_FOUND) and nothing of ordering's domain is imported here.
type OrderReader interface {
	GetOrder(context.Context, *connect.Request[orderingv1.GetOrderRequest]) (*connect.Response[orderingv1.GetOrderResponse], error)
}

// PublicService serves payment.v1.PublicPaymentService: the user-facing StartPayment and GetPayment.
// It decides nothing about payments: it finds out who is asking, whether the caller may see the order and
// whether payment is on for it, and then asks payment-rpc, which owns every rule about the payment itself.
type PublicService struct {
	payments  paymentv1connect.PaymentServiceHandler
	orders    OrderReader
	adminRole string
	enabled   bool // ordering.require_payment: rpc's setting, applied here to StartPayment only
}

// NewPublicService builds the service. adminRole is auth.admin_role (empty = the default).
func NewPublicService(payments paymentv1connect.PaymentServiceHandler, orders OrderReader, auth config.Auth, ordering config.Ordering) *PublicService {
	role := auth.AdminRole
	if role == "" {
		role = admin.DefaultAdminRole // config.Load sets the same; this is for hand-built configs (tests)
	}

	return &PublicService{payments: payments, orders: orders, adminRole: role, enabled: ordering.RequirePayment}
}

// Route returns the mount path and handler (Connect + gRPC + gRPC-Web on one port).
func (x *PublicService) Route(opts ...connect.HandlerOption) (string, http.Handler) {
	return paymentv1connect.NewPublicPaymentServiceHandler(x, opts...)
}

// caller is who the verified credential says is asking; nobody (no credential) cannot pay or read a payment.
func (x *PublicService) caller(c context.Context) (customer string, isAdmin bool, err error) {
	p := contextx.PrincipalFrom(c)
	if p.IsAnonymous() {
		ce := connect.NewError(connect.CodeUnauthenticated, errors.New("a credential is required to use payments"))
		ce.Meta().Set("x-error-code", unauthenticatedCode)

		return "", false, ce
	}

	return p.Subject, p.HasRole(x.adminRole), nil
}

func (x *PublicService) StartPayment(c context.Context, req *connect.Request[paymentv1.PublicPaymentServiceStartPaymentRequest]) (*connect.Response[paymentv1.PublicPaymentServiceStartPaymentResponse], error) {
	customer, isAdmin, err := x.caller(c)
	if err != nil {
		return nil, err
	}
	// The order, as this caller may see it: someone else's, or one that is not there, is NOT_FOUND from
	// ordering; any other failure of ordering is returned as it is, it is not "no order".
	got, err := x.orders.GetOrder(c, connect.NewRequest(&orderingv1.GetOrderRequest{Id: req.Msg.GetOrderId()}))
	if err != nil {
		return nil, err
	}
	if !x.enabled || !got.Msg.GetOrder().GetPaymentRequired() {
		ce := connect.NewError(connect.CodeFailedPrecondition, errors.New("payment is not available for this order"))
		ce.Meta().Set("x-error-code", DisabledCode)

		return nil, ce
	}
	res, err := x.payments.StartPayment(c, connect.NewRequest(&paymentv1.StartPaymentRequest{
		OrderId: req.Msg.GetOrderId(), CustomerId: customer, Admin: isAdmin,
	}))
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&paymentv1.PublicPaymentServiceStartPaymentResponse{Payment: res.Msg.GetPayment()}), nil
}

func (x *PublicService) GetPayment(c context.Context, req *connect.Request[paymentv1.PublicPaymentServiceGetPaymentRequest]) (*connect.Response[paymentv1.PublicPaymentServiceGetPaymentResponse], error) {
	customer, isAdmin, err := x.caller(c)
	if err != nil {
		return nil, err
	}
	res, err := x.payments.GetPayment(c, connect.NewRequest(&paymentv1.GetPaymentRequest{
		OrderId: req.Msg.GetOrderId(), CustomerId: customer, Admin: isAdmin,
	}))
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&paymentv1.PublicPaymentServiceGetPaymentResponse{Payment: res.Msg.GetPayment()}), nil
}
