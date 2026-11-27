package server_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

type schemalessRoute struct{}

func (schemalessRoute) Register(s *connect.Server) {
	s.Register(connect.Method{
		Spec:    connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.v1.Schemaless/Call"},
		Handler: func(context.Context, connect.Spec, connect.ServerStream) error { return nil },
	})
}

// A procedure with no method descriptor has no declaration to read: the server refuses to start, in every
// auth.mode, instead of serving it unchecked.
func TestStartup_RefusesProcedureWithoutDescriptor(t *testing.T) {
	tel, cleanup, err := otel.New(config.OTel{}, config.Log{Level: "error", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	keys := "ci=ci-key-0123456789abcdef"
	for _, mode := range []string{config.AuthOff, config.AuthOptional, config.AuthRequired} {
		t.Run(mode, func(t *testing.T) {
			_, err := server.New(config.HTTP{Addr: ":0"}, config.Auth{Mode: mode, APIKeys: keys},
				slog.New(slog.NewTextHandler(io.Discard, nil)), []server.Route{schemalessRoute{}}, nil, nil, tel, nil, server.OIDCAuth{})
			if err == nil || !strings.Contains(err.Error(), "/test.v1.Schemaless/Call") {
				t.Fatalf("server.New = %v, want a refusal naming /test.v1.Schemaless/Call", err)
			}
		})
	}
}
