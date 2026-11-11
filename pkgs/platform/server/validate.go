package server

import (
	"context"
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
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
func NewValidateInterceptor() (connect.ServerInterceptor, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("protovalidate: %w", err)
	}

	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(c context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return next(c, spec, &validatingStream{ServerStream: stream, v: v})
		}
	}, nil
}

// validatingStream checks every message the handler reads. A v2 interceptor sees a
// stream, not a request, so the check moves from "before the handler" to the
// handler's Receive: the same point in the call for a unary RPC (the handler reads
// its one request first), and it also covers each message of a streaming one.
type validatingStream struct {
	connect.ServerStream
	v protovalidate.Validator
}

func (s *validatingStream) Receive(msg any) error {
	if err := s.ServerStream.Receive(msg); err != nil {
		return err
	}
	if m, ok := msg.(proto.Message); ok {
		if err := s.v.Validate(m); err != nil {
			return toValidationError(err)
		}
	}

	return nil
}

func toValidationError(err error) error {
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) {
		// Compilation / runtime failure of a rule: the server is misconfigured.
		return connect.NewError(connect.CodeInternal, "request validation unavailable")
	}

	ce := connect.NewError(connect.CodeInvalidArgument, ve.Error())
	if detail, derr := connectproto.NewErrorDetail(ve.ToProto()); derr == nil {
		ce = ce.WithDetail(detail)
	}

	return WithMeta(ce, "x-error-code", ValidationErrorCode)
}
