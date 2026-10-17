package server

import (
	"context"
	"errors"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"buf.build/go/protovalidate"
	"connectrpc.com/connect"

	commonv1 "github.com/blackhorseya/x-monorepo/pkgs/platform/contract/common/v1"
)

func TestValidateInterceptor(t *testing.T) {
	interceptor, err := NewValidateInterceptor()
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	next := interceptor(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		called++
		return connect.NewResponse(&commonv1.Money{}), nil
	})

	// Valid message reaches the handler.
	if _, err := next(context.Background(), connect.NewRequest(&commonv1.Money{AmountMinor: 1, Currency: "USD"})); err != nil || called != 1 {
		t.Fatalf("valid: err=%v called=%d", err, called)
	}

	// Two violations in one message: InvalidArgument, the shared header, and
	// the violations as an error detail clients can decode.
	_, err = next(context.Background(), connect.NewRequest(&commonv1.Money{AmountMinor: -1, Currency: "usd"}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInvalidArgument || called != 1 {
		t.Fatalf("invalid: err=%v called=%d", err, called)
	}
	if got := ce.Meta().Get("x-error-code"); got != ValidationErrorCode {
		t.Fatalf("x-error-code = %q", got)
	}
	if len(ce.Details()) != 1 {
		t.Fatalf("details = %d, want 1", len(ce.Details()))
	}
	detail, err := ce.Details()[0].Value()
	if err != nil {
		t.Fatal(err)
	}
	violations, ok := detail.(*validate.Violations)
	if !ok || len(violations.GetViolations()) != 2 {
		t.Fatalf("detail = %T %v", detail, detail)
	}
	fields := map[string]bool{}
	for _, v := range violations.GetViolations() {
		fields[v.GetField().GetElements()[0].GetFieldName()] = true
	}
	if !fields["amount_minor"] || !fields["currency"] {
		t.Fatalf("violated fields = %v", fields)
	}
}

func TestToValidationError_NonValidationIsInternal(t *testing.T) {
	var ce *connect.Error
	if err := toValidationError(errors.New("cel compile failed")); !errors.As(err, &ce) || ce.Code() != connect.CodeInternal || ce.Meta().Get("x-error-code") != "" {
		t.Fatalf("err = %v", err)
	}
	var ve *protovalidate.ValidationError
	if !errors.As(toValidationError(&protovalidate.ValidationError{}), &ce) || ce.Code() != connect.CodeInvalidArgument || errors.As(ce, &ve) {
		t.Fatalf("validation error must become InvalidArgument without leaking the type")
	}
}
