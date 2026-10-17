// Package server hosts Connect/gRPC/gRPC-Web services on one port and is shared
// by every app. It knows nothing about any bounded context.
//
// gin is the HTTP router: Connect handlers are mounted under their service
// prefix with gin.WrapH, plain routes (/api/healthz, /api/metrics, /api/docs) are gin
// handlers. Request logging for RPCs stays in the Connect interceptor, which
// sees procedure and code; gin only contributes routing and panic recovery.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Everything that is not an RPC lives under /api so one ingress rule covers it
// (RPC paths are "/<pkg>.<Service>/…" and never collide).
const (
	HealthzPath = "/api/healthz"
	MetricsPath = "/api/metrics"
)

// Route is implemented by every BC's rpc adapter.
type Route interface {
	Route(opts ...connect.HandlerOption) (string, http.Handler)
}

// Server serves HTTP/1.1 + h2c (gRPC behind GKE/EKS ingress without TLS termination here).
type Server struct {
	srv    *http.Server
	logger *slog.Logger
}

// New mounts every route, /api/healthz, /api/metrics (Prometheus exposition of the
// OTel MeterProvider) and, when cfg.Docs is set, /api/docs. Every RPC gets an
// OpenTelemetry span (otelconnect, outermost) and a request logger carrying
// its trace_id and request_id, and every response carries X-Request-Id /
// X-Trace-Id (see meta.go); plain HTTP routes (docs, openapi) get spans from otelgin,
// which skips RPC prefixes, /api/healthz and /api/metrics so nothing is traced twice.
func New(cfg config.HTTP, logger *slog.Logger, routes []Route, docs []Doc, tel *otel.Providers) (*Server, error) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// Connect routes are "/<pkg>.<Service>/" prefixes; a procedure is the next segment.
	// Redirecting or cleaning paths would break gRPC clients, so keep paths verbatim.
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false

	tracing, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tel.Tracer),
		otelconnect.WithMeterProvider(tel.Meter),
		otelconnect.WithPropagator(tel.Propagator),
		otelconnect.WithPropagateResponseHeader(), // traceparent on responses too
	)
	if err != nil {
		return nil, fmt.Errorf("otelconnect interceptor: %w", err)
	}
	validate, err := NewValidateInterceptor()
	if err != nil {
		return nil, err
	}
	// Outermost first: validation is innermost so a rejected request is still
	// traced, logged (code invalid_argument) and carries X-Request-Id.
	opts := connect.WithInterceptors(tracing, NewMetaInterceptor(), NewLoggingInterceptor(logger), validate)
	prefixes := make([]string, 0, len(routes))
	handlers := make([]http.Handler, 0, len(routes))
	for _, r := range routes {
		prefix, h := r.Route(opts)
		prefixes = append(prefixes, prefix)
		handlers = append(handlers, h)
	}

	// Middleware must be registered before the routes it applies to.
	engine.Use(
		otelgin.Middleware(tel.ServiceName,
			otelgin.WithTracerProvider(tel.Tracer),
			otelgin.WithMeterProvider(tel.Meter),
			otelgin.WithPropagators(tel.Propagator),
			otelgin.WithGinFilter(untracedByGin(prefixes)),
		),
		gin.Recovery(),
	)
	for i, prefix := range prefixes {
		engine.Any(prefix+"*procedure", gin.WrapH(handlers[i]))
	}
	engine.GET(HealthzPath, func(c *gin.Context) { c.Status(http.StatusOK) })
	engine.GET(MetricsPath, gin.WrapH(tel.MetricsHandler))
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

// untracedByGin returns the otelgin filter: false for paths otelconnect
// already traces (RPC prefixes) and for the scrape/probe endpoints.
func untracedByGin(prefixes []string) func(*gin.Context) bool {
	return func(c *gin.Context) bool {
		path := c.Request.URL.Path
		if path == HealthzPath || path == MetricsPath {
			return false
		}
		for _, p := range prefixes {
			if strings.HasPrefix(path, p) {
				return false
			}
		}

		return true
	}
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
