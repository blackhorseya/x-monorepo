// Package client is the payment service seen from another process: a Connect client for payment-rpc that
// implements paymentv1connect.PaymentServiceHandler, plus PublicService, the payment API apps/rpc serves
// to users (payment.v1.PublicPaymentService) on top of it. apps/rpc imports this package and payment's
// contract and nothing else of the BC: the payment domain is not loaded into rpc (docs/payment.md §1, the
// way #320 did for the catalog), and payment's own provider.go is not imported either.
//
// Three rules live here and nowhere else:
//
//   - Credentials. The only credential that reaches payment-rpc is the service key from config.Payment.
//     Each call builds a fresh request from the message alone, so nothing the caller presented (a user's
//     X-API-Key, a bearer token, a cookie) is forwarded. Who the user is travels as customer_id / admin in
//     the message, filled by PublicService from the verified credential, never from the client's body.
//   - Failures. A business answer of payment-rpc, recognised by an x-error-code of its own (payment.*),
//     passes through unchanged. Everything else, payment-rpc not answering for any reason or answering
//     with something that is not its own business answer, is one error: UNAVAILABLE with x-error-code
//     payment.unavailable and a fixed message. The cause goes to the log as a fixed word, never as what
//     the upstream wrote (its text may reflect a credential, #347).
//   - Optional. Payment is not deployed until slice 5, so unconfigured is a legal state of rpc: every call
//     is then payment.unavailable. A half-configured one, or the switch on without payment-rpc, is a
//     start-up error (New).
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/contextx"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

var _ paymentv1connect.PaymentServiceHandler = (*Client)(nil)

const (
	// UnavailableCode is the x-error-code of payment-rpc not answering (or not answering as itself).
	UnavailableCode = "payment.unavailable"
	// ProviderUnavailableCode is payment-rpc's own answer to StartPayment when its provider failed.
	ProviderUnavailableCode = "payment.provider_unavailable"
	// APIKeyHeader carries the service key (the header payment-rpc's auth interceptor reads).
	APIKeyHeader = "X-API-Key"
)

// businessCode is what a payment-rpc business answer carries; anything else is not trusted as one.
var businessCode = regexp.MustCompile(`^payment\.[a-z_]{1,48}$`)

// Client asks payment-rpc over Connect. A zero cc is the unconfigured state: every call is unavailable.
type Client struct {
	cc      paymentv1connect.PaymentServiceClient
	timeout time.Duration
}

// New builds the client for cfg. With nothing configured it returns the unconfigured client, unless
// requirePayment (ordering.require_payment) is on: the switch on without a payment-rpc to ask is a
// mistake, and rpc refuses to start. A payment.upstream without a payment.api_key, or the other way
// round, or one that is not an http(s) URL, or a non-positive payment.timeout, is refused whether or not
// the switch is on. No error carries the key.
func New(cfg config.Payment, ordering config.Ordering, tel *otel.Providers) (*Client, error) {
	if err := config.CheckRequirePayment(ordering, cfg); err != nil {
		return nil, fmt.Errorf("payment client: %w", err)
	}
	if cfg.Upstream == "" && cfg.APIKey == "" {
		return &Client{}, nil
	}
	u, err := url.Parse(cfg.Upstream)
	if cfg.Upstream == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("payment client: payment.upstream must be an http(s) URL")
	}
	if u.User != nil {
		return nil, errors.New("payment client: payment.upstream must not carry credentials (userinfo): the key is APP_PAYMENT_API_KEY")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("payment client: payment.api_key (APP_PAYMENT_API_KEY) is empty")
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("payment client: payment.timeout %s must be > 0", cfg.Timeout)
	}
	tracing, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tel.Tracer),
		otelconnect.WithMeterProvider(tel.Meter),
		otelconnect.WithPropagator(tel.Propagator),
	)
	if err != nil {
		return nil, fmt.Errorf("payment client: otel interceptor: %w", err)
	}
	key := cfg.APIKey
	withKey := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(APIKeyHeader, key)

			return next(c, req)
		}
	})

	return &Client{
		cc: paymentv1connect.NewPaymentServiceClient(&http.Client{}, cfg.Upstream,
			connect.WithInterceptors(tracing, withKey)),
		timeout: cfg.Timeout,
	}, nil
}

func call[Req, Res any](
	x *Client, c context.Context, op string, in *Req,
	do func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
) (*connect.Response[Res], error) {
	if x.cc == nil {
		contextx.FromContext(c).Warn("payment call refused: payment-rpc is not configured", "op", op)

		return nil, unavailable()
	}
	cc, cancel := context.WithTimeout(c, x.timeout)
	defer cancel()

	out, err := do(cc, connect.NewRequest(in)) // a fresh request: the caller's headers stay behind
	if err == nil {
		return out, nil
	}

	return nil, classify(c, op, err)
}

// classify is the failure matrix. c is the caller's context (not the per-call one).
func classify(c context.Context, op string, err error) error {
	if errors.Is(c.Err(), context.Canceled) {
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeFailedPrecondition,
			connect.CodePermissionDenied:
			if businessCode.MatchString(ce.Meta().Get("x-error-code")) {
				return clean(ce) // payment-rpc's own answer: code, message, details and x-error-code
			}
		}
		if ce.Code() == connect.CodeUnavailable && ce.Meta().Get("x-error-code") == ProviderUnavailableCode {
			// payment-rpc is up and its provider is not: its own answer (nothing was changed, retry), the
			// code kept, the sentence ours: what the provider said is not for the caller (#347).
			out := connect.NewError(connect.CodeUnavailable, errors.New("the payment provider could not be reached, retry the request"))
			out.Meta().Set("x-error-code", ProviderUnavailableCode)

			return out
		}
		if ce.Code() == connect.CodeUnauthenticated || ce.Code() == connect.CodePermissionDenied {
			contextx.FromContext(c).Error("payment service credential rejected: check APP_PAYMENT_API_KEY against payment-rpc's auth.api_keys",
				"op", op, "code", ce.Code().String())

			return unavailable()
		}
		// The log carries the operation, a fixed line and the Connect code, never what the remote wrote
		// (its message may reflect a credential) and never the transport error (it carries the address).
		contextx.FromContext(c).Warn("payment call failed", "op", op, "code", ce.Code().String())

		return unavailable()
	}
	contextx.FromContext(c).Warn("payment call failed", "op", op, "reason", reasonOf(err))

	return unavailable()
}

// clean keeps what the caller may see of an upstream business error: code, message, details and
// x-error-code. The hop's transport headers do not follow it (the same rule as the catalog forwarder).
func clean(ce *connect.Error) error {
	out := connect.NewError(ce.Code(), errors.New(ce.Message()))
	for _, d := range ce.Details() {
		out.AddDetail(d)
	}
	out.Meta().Set("x-error-code", ce.Meta().Get("x-error-code"))

	return out
}

// reasonOf names a transport failure that is not a Connect error with a fixed word.
func reasonOf(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case errors.As(err, &ne):
		return "unreachable"
	default:
		return "transport"
	}
}

func unavailable() error {
	ce := connect.NewError(connect.CodeUnavailable, errors.New("payment could not be reached, retry the request"))
	ce.Meta().Set("x-error-code", UnavailableCode)

	return ce
}

func (x *Client) StartPayment(c context.Context, req *connect.Request[paymentv1.StartPaymentRequest]) (*connect.Response[paymentv1.StartPaymentResponse], error) {
	return call(x, c, "StartPayment", req.Msg, func(c context.Context, r *connect.Request[paymentv1.StartPaymentRequest]) (*connect.Response[paymentv1.StartPaymentResponse], error) {
		return x.cc.StartPayment(c, r) // looked up here: an unconfigured client has no cc, and call refuses first
	})
}

func (x *Client) GetPayment(c context.Context, req *connect.Request[paymentv1.GetPaymentRequest]) (*connect.Response[paymentv1.GetPaymentResponse], error) {
	return call(x, c, "GetPayment", req.Msg, func(c context.Context, r *connect.Request[paymentv1.GetPaymentRequest]) (*connect.Response[paymentv1.GetPaymentResponse], error) {
		return x.cc.GetPayment(c, r)
	})
}
