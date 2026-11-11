// Package server hosts Connect/gRPC/gRPC-Web services on one port and is shared
// by every app. It knows nothing about any bounded context.
//
// gin is the HTTP router: Connect handlers are mounted under their service
// prefix with gin.WrapH, both at the protocol path "/<pkg>.<Service>/…" and
// under RPCPrefix ("/api/<pkg>.<Service>/…"); plain routes (/api/healthz,
// /api/readyz, /api/metrics, /api/docs) are gin handlers. Request logging for RPCs stays in the Connect interceptor, which
// sees procedure and code; gin only contributes routing and panic recovery.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/buildinfo"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// Everything lives under /api so one ingress rule covers the whole server.
//
// RPCs are served twice: at RPCPrefix + "/<pkg>.<Service>/<Method>", which is
// what Connect / gRPC-Web clients configured with base URL "…/api" call (the
// console, curl, Scalar), and at the bare "/<pkg>.<Service>/<Method>", because
// the gRPC protocol fixes that path and a grpc-go / grpcurl client cannot add
// a prefix. The handler is the same; only the route differs.
const (
	RPCPrefix   = "/api"
	MetricsPath = "/api/metrics"
)

// defaultShutdownTimeout is http.shutdown_timeout when unset (config.Load sets the same).
const defaultShutdownTimeout = 15 * time.Second

// Route is implemented by every BC's rpc adapter: it registers its services on the
// connect.Server that carries the interceptors below (the generated
// Register<Service>Handler does the work).
type Route interface {
	Register(s *connect.Server)
}

// Option is a composition-root decision about the server, made in code and
// supplied by wire; none of them is a config key or an env var, so an operator
// has no way to change one.
type Option func(*options)

type options struct {
	trustRemoteTrace bool
	httpRoutes       []httpRoute
}

type httpRoute struct {
	method, path string
	handler      http.Handler
}

// WithHTTPRoute mounts a plain HTTP handler (not Connect) at method + path, next to the probes. It is
// for the few things that are not an RPC of ours, such as an external provider's webhook.
//
// Know what it does NOT get: the auth interceptor, the rate limit and the timeout wrap Connect procedures
// only, so a handler mounted here is open to whoever can reach the port unless it authenticates the request
// itself (a webhook verifies a signature over the raw body, before it writes anything). The route does get
// the request span (otelgin) and gin's recovery. Paths under a Connect prefix are not for this.
func WithHTTPRoute(method, path string, h http.Handler) Option {
	return func(o *options) {
		o.httpRoutes = append(o.httpRoutes, httpRoute{method: method, path: path, handler: h})
	}
}

// TrustRemoteTrace makes the server continue the trace named by an incoming
// traceparent: the RPC's server span is a child of the caller's span and shares
// its trace_id. Only a service whose callers are all trusted may say this
// (a service reachable from one known caller alone, by NetworkPolicy and a service key),
// because the caller then picks the trace_id of everything logged and traced
// under that request. The default, and what a service facing outside callers
// must keep, is DistrustRemoteTrace.
func TrustRemoteTrace() Option { return func(o *options) { o.trustRemoteTrace = true } }

// DistrustRemoteTrace is the default, spelled out for composition roots to
// declare: an incoming traceparent is not continued. The RPC's server span
// starts a new root trace and keeps the caller's span only as a link, so the
// X-Trace-Id of the response is this service's own.
func DistrustRemoteTrace() Option { return func(o *options) { o.trustRemoteTrace = false } }

// Server serves HTTP/1.1 + h2c (gRPC behind GKE/EKS ingress without TLS termination here).
type Server struct {
	srv             *http.Server
	logger          *slog.Logger
	health          *health
	shutdownDelay   time.Duration
	shutdownTimeout time.Duration
}

// New mounts every route, the probes (/api/healthz liveness, /api/readyz
// readiness over checks, see health.go), /api/metrics (Prometheus exposition
// of the OTel MeterProvider) and, when cfg.Docs is set, /api/docs. Every RPC gets an
// OpenTelemetry span (otelconnect, outermost) and a request logger carrying
// its trace_id and request_id, and every response carries X-Request-Id, and
// X-Trace-Id when there is a trace (not when otel.endpoint is empty; see meta.go); the rate limit (limits.go, when cfg.RateLimit.RPS
// is set) drops a flood; auth (auth.go, when auth.Mode is not off) resolves
// the caller to a contextx.Principal; the timeout (limits.go, cfg.Timeout)
// bounds the rest (and a call that outlived it is refused before the handler, refuseExpired); opts (TrustRemoteTrace / DistrustRemoteTrace) decide whether
// an incoming traceparent is continued, and none means it is not; plain HTTP routes (docs, openapi) get spans from otelgin,
// which skips RPC prefixes, the probes and /api/metrics so nothing is traced twice.
func New(cfg config.HTTP, auth config.Auth, logger *slog.Logger, routes []Route, docs []Doc, checks Readiness, tel *otel.Providers, resolver CredentialResolver, oidcAuth OIDCAuth, opts ...Option) (*Server, error) {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	// Connect routes are "/<pkg>.<Service>/" prefixes; a procedure is the next segment.
	// Redirecting or cleaning paths would break gRPC clients, so keep paths verbatim.
	engine.RedirectTrailingSlash = false
	engine.RedirectFixedPath = false

	var o options
	for _, opt := range opts {
		opt(&o)
	}
	traceOpts := []otelconnect.Option{
		otelconnect.WithTracerProvider(tel.Tracer),
		otelconnect.WithMeterProvider(tel.Meter),
		otelconnect.WithPropagator(tel.Propagator),
		otelconnect.WithPropagateResponseHeader(), // traceparent on responses too
	}
	if o.trustRemoteTrace {
		// otelconnect's default is a new root trace with the caller's span as a link.
		traceOpts = append(traceOpts, otelconnect.WithTrustRemote())
	}
	tracing, err := otelconnect.NewServerInterceptor(traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("otelconnect interceptor: %w", err)
	}
	validate, err := NewValidateInterceptor()
	if err != nil {
		return nil, err
	}
	authn, err := NewAuthInterceptor(auth, tel.Meter, resolver, oidcAuth)
	if err != nil {
		return nil, err
	}
	limiter, err := NewRateLimitInterceptor(cfg.RateLimit)
	if err != nil {
		return nil, err
	}
	// Outermost first: everything that rejects is inside logging so a
	// rejected request is still traced, logged (its code) and carries
	// X-Request-Id. Rate limit before auth so a flood is dropped before each
	// request pays for a credential check; timeout after auth so the
	// deadline covers validation and the use case, not credential parsing;
	// auth before validation so an anonymous caller learns nothing about
	// the request rules.
	interceptors := []connect.ServerInterceptor{tracing, dropHeadersOnError(tel.Propagator.Fields()...), NewMetaInterceptor(), NewLoggingInterceptor(logger)}
	if limiter != nil {
		interceptors = append(interceptors, limiter)
		logger.Info("rate limit enabled", "rps", cfg.RateLimit.RPS, "burst", cfg.RateLimit.Burst, "by", cfg.RateLimit.By)
	}
	if authn != nil {
		interceptors = append(interceptors, authn)
		logger.Info("auth enabled", "mode", auth.Mode, "public", auth.Public)
		if auth.APIKeys == "" && auth.JWT.HS256Secret == "" && auth.JWT.PublicKey == "" {
			// Valid (the resolver alone can answer), but with an empty identity
			// store nobody can ever get in: boot would have failed here before
			// resolvers existed, so say it loudly instead of locking out silently.
			logger.Warn("auth relies solely on resolver-managed api keys: no auth.api_keys and no auth.jwt — bootstrap needs a static admin key or an existing identity database")
		}
	}
	if deadline := NewTimeoutInterceptor(cfg.Timeout); deadline != nil {
		interceptors = append(interceptors, deadline)
	}
	rpc := connect.NewServer(append(interceptors, validate, refuseExpired)...)
	for _, r := range routes {
		r.Register(rpc)
	}
	// Every RPC is mounted by its exact procedure. A path under a service prefix that is
	// no procedure of ours answers 404 without running an interceptor (what v1's
	// generated handler did), not Unimplemented through the chain (what connecthttp.Mount's
	// per-service catch-all would do).
	mux := http.NewServeMux()
	// One request message is read up to connect-go v2's default, 4 MiB (not a setting); a larger one
	// is refused with resource_exhausted (readlimit_test.go).
	connecthttp.Mount(mux, rpc)
	procedures := map[string]bool{}
	var prefixes []string
	for spec := range rpc.Specs() {
		procedures[spec.Procedure] = true
		if i := strings.LastIndexByte(spec.Procedure, '/'); i > 0 && !slices.Contains(prefixes, spec.Procedure[:i+1]) {
			prefixes = append(prefixes, spec.Procedure[:i+1])
		}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !procedures[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

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
	for _, prefix := range prefixes {
		engine.Any(prefix+"*procedure", gin.WrapH(handler))
		engine.Any(RPCPrefix+prefix+"*procedure", gin.WrapH(http.StripPrefix(RPCPrefix, handler)))
	}
	hp := &health{checks: checks}
	engine.GET(HealthzPath, hp.live)
	engine.GET(ReadyzPath, hp.ready)
	engine.GET(VersionPath, hp.version)
	engine.GET(MetricsPath, gin.WrapH(tel.MetricsHandler))
	for _, r := range o.httpRoutes {
		engine.Handle(r.method, r.path, gin.WrapH(r.handler))
	}
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

	// A config built by hand (tests) skips config.Load's defaults.
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}

	return &Server{
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           engine,
			Protocols:         protocols,
			ReadHeaderTimeout: 5 * time.Second,
		},
		logger:          logger,
		health:          hp,
		shutdownDelay:   cfg.ShutdownDelay,
		shutdownTimeout: cfg.ShutdownTimeout,
	}, nil
}

// untracedByGin returns the otelgin filter: false for paths otelconnect
// already traces (RPC prefixes) and for the scrape/probe endpoints.
func untracedByGin(prefixes []string) func(*gin.Context) bool {
	return func(c *gin.Context) bool {
		path := c.Request.URL.Path
		if path == HealthzPath || path == ReadyzPath || path == VersionPath || path == MetricsPath {
			return false
		}
		for _, p := range prefixes {
			if strings.HasPrefix(path, p) || strings.HasPrefix(path, RPCPrefix+p) {
				return false
			}
		}

		return true
	}
}

// Handler exposes the router for in-process tests.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Run listens on cfg.Addr and serves until c is cancelled (Serve).
func (s *Server) Run(c context.Context) error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.srv.Addr, err)
	}

	return s.Serve(c, ln)
}

// Serve serves on ln until c is cancelled (SIGTERM in main), then shuts
// down in the order a load balancer needs:
//
//  1. /api/readyz answers 503 "draining" while everything else keeps being
//     served for http.shutdown_delay, so a balancer polling readiness stops
//     routing here before the listener goes away (0 skips the wait);
//  2. the listener closes and in-flight requests get http.shutdown_timeout
//     to finish; the error is that deadline's when some did not.
//
// Liveness stays 200 throughout: the process is fine, it is leaving.
// It returns nil after a clean drain, or the listen/serve error.
func (s *Server) Serve(c context.Context, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("server listening", append([]any{"addr", ln.Addr().String()}, buildinfo.Get().LogAttrs()...)...)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-c.Done():
	}

	s.health.draining.Store(true)
	s.logger.Info("server draining", "delay", s.shutdownDelay, "timeout", s.shutdownTimeout)
	if s.shutdownDelay > 0 {
		time.Sleep(s.shutdownDelay)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: in-flight requests outlived http.shutdown_timeout %s: %w", s.shutdownTimeout, err)
	}
	s.logger.Info("server stopped")

	return nil
}
