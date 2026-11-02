package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/client"
	paymentv1 "github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/contract/v1/paymentv1connect"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/postgres/pgtest"
)

const key = "payment-service-key-0123456789"

// upstream is a payment-rpc whose answer to StartPayment is whatever the test says.
type upstream struct {
	paymentv1connect.UnimplementedPaymentServiceHandler
	answer func() (*paymentv1.Payment, error)
	key    string
}

func (u *upstream) StartPayment(_ context.Context, req *connect.Request[paymentv1.StartPaymentRequest]) (*connect.Response[paymentv1.StartPaymentResponse], error) {
	u.key = req.Header().Get(client.APIKeyHeader)
	p, err := u.answer()
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&paymentv1.StartPaymentResponse{Payment: p}), nil
}

func newClient(t *testing.T, u *upstream) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(paymentv1connect.NewPaymentServiceHandler(u))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	c, err := client.New(config.Payment{Upstream: ts.URL, APIKey: key, Timeout: time.Second}, config.Ordering{}, pgtest.Telemetry(t))
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func coded(code connect.Code, xcode, msg string) error {
	ce := connect.NewError(code, errors.New(msg))
	if xcode != "" {
		ce.Meta().Set("x-error-code", xcode)
	}

	return ce
}

// What the caller gets of whatever payment-rpc answered: its business answers (an x-error-code of its own
// under payment.) unchanged, and every other failure, whatever its Connect code and whatever it wrote,
// one fixed UNAVAILABLE payment.unavailable.
func TestClient_Failures(t *testing.T) {
	const leak = "SECRET-d41d8cd98f00b204e9800998ecf8427e"
	for _, tc := range []struct {
		name  string
		err   error
		code  connect.Code
		xcode string
		msg   string // "" = the message is the fixed one and carries no upstream text
	}{
		{"not found is the upstream's", coded(connect.CodeNotFound, "payment.not_found", "no such payment"), connect.CodeNotFound, "payment.not_found", "no such payment"},
		{"precondition is the upstream's", coded(connect.CodeFailedPrecondition, "payment.not_pending", "already paid"), connect.CodeFailedPrecondition, "payment.not_pending", "already paid"},
		{"permission denied with its own code is the upstream's", coded(connect.CodePermissionDenied, "payment.permission_denied", "no"), connect.CodePermissionDenied, "payment.permission_denied", "no"},
		{"a business code that is not payment's is not trusted", coded(connect.CodeFailedPrecondition, leak, leak), connect.CodeUnavailable, client.UnavailableCode, ""},
		{"a business code without any x-error-code is not trusted", coded(connect.CodeNotFound, "", leak), connect.CodeUnavailable, client.UnavailableCode, ""},
		{"a rejected service key is an outage to the caller", coded(connect.CodeUnauthenticated, "auth.invalid_credential", leak), connect.CodeUnavailable, client.UnavailableCode, ""},
		{"a service key without the role is an outage to the caller", coded(connect.CodePermissionDenied, "auth.permission_denied", leak), connect.CodeUnavailable, client.UnavailableCode, ""},
		{"internal error", coded(connect.CodeInternal, leak, leak), connect.CodeUnavailable, client.UnavailableCode, ""},
		{"the provider's failure keeps its code, with our sentence", coded(connect.CodeUnavailable, client.ProviderUnavailableCode, leak), connect.CodeUnavailable, client.ProviderUnavailableCode, ""},
		{"UNAVAILABLE with any other code is just an outage", coded(connect.CodeUnavailable, "payment.not_pending", leak), connect.CodeUnavailable, client.UnavailableCode, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &upstream{answer: func() (*paymentv1.Payment, error) { return nil, tc.err }}
			_, err := newClient(t, u).StartPayment(context.Background(), connect.NewRequest(&paymentv1.StartPaymentRequest{OrderId: "o", CustomerId: "c"}))
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != tc.code || ce.Meta().Get("x-error-code") != tc.xcode {
				t.Fatalf("err = %v, want %s / %s", err, tc.code, tc.xcode)
			}
			if u.key != key {
				t.Fatalf("payment-rpc saw key %q, want the service key (positive control: it was asked)", u.key)
			}
			if tc.msg != "" && ce.Message() != tc.msg {
				t.Fatalf("message = %q, want the upstream's %q", ce.Message(), tc.msg)
			}
			if tc.msg == "" && strings.Contains(err.Error(), leak) {
				t.Fatalf("what the upstream wrote reached the caller: %v", err)
			}
		})
	}
}

func TestClient_AHangIsUnavailableWithinTheBudget(t *testing.T) {
	u := &upstream{answer: func() (*paymentv1.Payment, error) { time.Sleep(3 * time.Second); return &paymentv1.Payment{}, nil }}
	start := time.Now()
	_, err := newClient(t, u).StartPayment(context.Background(), connect.NewRequest(&paymentv1.StartPaymentRequest{OrderId: "o", CustomerId: "c"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Meta().Get("x-error-code") != client.UnavailableCode {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("took %s, want about the 1s budget", d)
	}
}

// Nothing configured is a legal state (payment is not deployed yet): every call is payment.unavailable.
// The switch on, or a half-configured payment, is not.
func TestNew_Configurations(t *testing.T) {
	tel := pgtest.Telemetry(t)
	const k = "a-payment-service-key-0123456789abcdef"
	for _, tc := range []struct {
		name string
		cfg  config.Payment
		ord  config.Ordering
		ok   bool
	}{
		{"nothing", config.Payment{Timeout: time.Second}, config.Ordering{}, true},
		{"switch on, nothing", config.Payment{Timeout: time.Second}, config.Ordering{RequirePayment: true}, false},
		{"upstream only", config.Payment{Upstream: "http://payment-rpc:80", Timeout: time.Second}, config.Ordering{}, false},
		{"key only", config.Payment{APIKey: k, Timeout: time.Second}, config.Ordering{}, false},
		{"not a URL", config.Payment{Upstream: "payment-rpc", APIKey: k, Timeout: time.Second}, config.Ordering{}, false},
		{"userinfo in the URL", config.Payment{Upstream: "http://u:p@payment-rpc:80", APIKey: k, Timeout: time.Second}, config.Ordering{}, false},
		{"no budget", config.Payment{Upstream: "http://payment-rpc:80", APIKey: k}, config.Ordering{}, false},
		{"complete", config.Payment{Upstream: "http://payment-rpc:80", APIKey: k, Timeout: time.Second}, config.Ordering{RequirePayment: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := client.New(tc.cfg, tc.ord, tel)
			if (err == nil) != tc.ok {
				t.Fatalf("New = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), k) {
				t.Fatalf("the refusal carries the key: %v", err)
			}
			if tc.name == "nothing" {
				_, err := c.GetPayment(context.Background(), connect.NewRequest(&paymentv1.GetPaymentRequest{OrderId: "o", CustomerId: "c"}))
				var ce *connect.Error
				if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Meta().Get("x-error-code") != client.UnavailableCode {
					t.Fatalf("unconfigured GetPayment = %v, want payment.unavailable", err)
				}
			}
		})
	}
}

// The public requests have nowhere to write who the caller is: no customer_id, no admin. rpc takes both from
// the verified credential (PublicService); a field here would be a way for a client to claim them. Positive
// control: the service has its two methods and each request carries the order id.
func TestPublicContract_HasNoIdentityFields(t *testing.T) {
	svc := paymentv1.File_payment_v1_public_proto.Services().ByName("PublicPaymentService")
	if svc == nil || svc.Methods().Len() != 2 {
		t.Fatalf("PublicPaymentService = %v, want its two methods", svc)
	}
	for i := range svc.Methods().Len() {
		in := svc.Methods().Get(i).Input()
		if in.Fields().ByName(protoreflect.Name("order_id")) == nil {
			t.Fatalf("%s has no order_id (positive control)", in.FullName())
		}
		for _, banned := range []protoreflect.Name{"customer_id", "admin"} {
			if in.Fields().ByName(banned) != nil {
				t.Errorf("%s has a field %s: who the caller is must come from the credential, not the body", in.FullName(), banned)
			}
		}
	}
}
