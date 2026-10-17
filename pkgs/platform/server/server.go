// Package server hosts Connect/gRPC/gRPC-Web services on one port and is shared
// by every app. It knows nothing about any bounded context.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
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

// New mounts every route, /healthz, /metrics (Prometheus text format from
// gatherer) and, when cfg.Docs is set, /docs.
func New(cfg config.HTTP, logger *slog.Logger, routes []Route, docs []Doc, gatherer prometheus.Gatherer) *Server {
	mux := http.NewServeMux()
	opts := connect.WithInterceptors(NewLoggingInterceptor(logger))
	for _, r := range routes {
		path, h := r.Route(opts)
		mux.Handle(path, h)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	if cfg.Docs {
		mountDocs(mux, docs)
		logger.Info("api docs enabled", "path", "/docs")
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	return &Server{
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           mux,
			Protocols:         protocols,
			ReadHeaderTimeout: 5 * time.Second,
		},
		logger: logger,
	}
}

// Handler exposes the mux for in-process tests.
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
