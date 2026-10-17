package server

import (
	"context"
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// ValidationErrorCode is the x-error-code of a request rejected by the
// protovalidate interceptor. The violations (field path, rule id, message)
// travel as a buf.validate.Violations error detail.
const ValidationErrorCode = "request.validation_failed"

// NewValidateInterceptor rejects requests whose message breaks the
// buf.validate rules declared in the .proto files, before any use case runs.
// It covers shape only (required, min_len, gte, pattern …); business
// invariants stay in the domain, which the consumer path (no interceptor)
// relies on. A rule that fails to compile is a programming error and surfaces
// as Internal, never as a client error.
func NewValidateInterceptor() (connect.UnaryInterceptorFunc, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("protovalidate: %w", err)
	}

	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			msg, ok := req.Any().(proto.Message)
			if !ok {
				return next(c, req)
			}
			if err := v.Validate(msg); err != nil {
				return nil, toValidationError(err)
			}

			return next(c, req)
		}
	}, nil
}

func toValidationError(err error) error {
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) {
		// Compilation / runtime failure of a rule: the server is misconfigured.
		return connect.NewError(connect.CodeInternal, errors.New("request validation unavailable"))
	}

	ce := connect.NewError(connect.CodeInvalidArgument, errors.New(ve.Error()))
	ce.Meta().Set("x-error-code", ValidationErrorCode)
	if detail, derr := connect.NewErrorDetail(ve.ToProto()); derr == nil {
		ce.AddDetail(detail)
	}

	return ce
}
