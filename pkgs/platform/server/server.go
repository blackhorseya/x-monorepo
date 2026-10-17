// Package server hosts Connect/gRPC/gRPC-Web services on one port and is shared
// by every app. It knows nothing about any bounded context.
//
// gin is the HTTP router: Connect handlers are mounted under their service
// prefix with gin.WrapH, plain routes (/healthz, /metrics, /api/docs) are gin
// handlers. Request logging for RPCs stays in the Connect interceptor, which
// sees procedure and code; gin only contributes routing and panic recovery.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/gin-gonic/gin"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Route is implemented by every BC's rpc adapter.
type Route interface {
	Route(opts ...connect.HandlerOption) (string, http.Handler)
}

// NewLogger builds the root logger.
func NewLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// Server serves HTTP/1.1 + h2c (gRPC behind GKE/EKS ingress without TLS termination here).
type Server struct {
	srv    *http.Server
	logger *slog.Logger
}

// New mounts every route, /healthz, /metrics (Prometheus exposition of the
// OTel MeterProvider) and, when cfg.Docs is set, /api/docs. Every RPC gets an
// OpenTelemetry span (otelconnect, outermost) and a request logger carrying
// its trace_id.
func New(cfg config.HTTP, logger *slog.Logger, routes []Route, docs []Doc, tel *otel.Providers) (*Server, error) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Recovery())
	// Connect routes are "/<pkg>.<Service>/" prefixes; a procedure is the next segment.
	// Redirecting or cleaning paths would break gRPC clients, so keep paths verbatim.
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false

	tracing, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tel.Tracer),
		otelconnect.WithMeterProvider(tel.Meter),
		otelconnect.WithPropagator(tel.Propagator),
	)
	if err != nil {
		return nil, fmt.Errorf("otelconnect interceptor: %w", err)
	}
	opts := connect.WithInterceptors(tracing, NewLoggingInterceptor(logger))
	for _, r := range routes {
		prefix, h := r.Route(opts)
		engine.Any(prefix+"*procedure", gin.WrapH(h))
	}
	engine.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET("/metrics", gin.WrapH(tel.MetricsHandler))
	if cfg.Docs {
		mountDocs(engine, docs)
		logger.Info("api docs enabled", "path", DocsPath)
	} else {
		engine.GET(DocsPath, func(c *gin.Context) {
			c.String(http.StatusNotFound, "api docs are disabled; start with APP_HTTP_DOCS=true\n")
		})
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	return &Server{
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           engine,
			Protocols:         protocols,
			ReadHeaderTimeout: 5 * time.Second,
		},
		logger: logger,
	}, nil
}

// Handler exposes the router for in-process tests.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Run blocks until c is cancelled, then shuts down gracefully.
func (s *Server) Run(c context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("server listening", "addr", s.srv.Addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-c.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.logger.Info("server shutting down")

	return s.srv.Shutdown(shutdownCtx)
}
