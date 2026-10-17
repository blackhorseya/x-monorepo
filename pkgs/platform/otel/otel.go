// Package otel owns the telemetry of a process: the OpenTelemetry providers
// and the root slog.Logger. Metrics are always on: a Prometheus reader serves
// them at /metrics (scrape without a collector). With cfg.Endpoint set, traces,
// metrics and logs are also pushed over OTLP/gRPC (locally: grafana/otel-lgtm
// from deployments/docker — Tempo, Prometheus, Loki).
//
// Nothing is global: apps wire ProviderSet and components receive the
// providers, so every Initialize() — tests included — gets its own.
package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/wire"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// ProviderSet wires New and exposes the MeterProvider (usecasex.NewRecorder)
// and the root *slog.Logger without the rest of Providers.
var ProviderSet = wire.NewSet(
	New,
	wire.FieldsOf(new(*Providers), "Meter", "Logger"),
)

// Providers is what instrumentation (otelconnect, usecasex, …) consumes.
type Providers struct {
	Tracer     trace.TracerProvider
	Meter      metric.MeterProvider
	Propagator propagation.TextMapPropagator
	// Logger is the root logger: stdout, plus OTLP (Loki) when exporting.
	Logger *slog.Logger
	// MetricsHandler serves the Prometheus exposition of Meter (GET /metrics).
	MetricsHandler http.Handler
}

const (
	shutdownTimeout = 5 * time.Second
	scopeName       = "github.com/blackhorseya/x-monorepo/pkgs/platform/otel"
)

// New builds the providers and the root logger; the cleanup flushes and shuts
// the exporters down.
func New(cfg config.OTel, logCfg config.Log) (*Providers, func(), error) {
	c := context.Background()
	res, err := newResource(c, cfg)
	if err != nil {
		return nil, nil, err
	}

	// Prometheus exposition is always available. Scope labels are dropped so the
	// series look like hand-written Prometheus metrics ({kind, usecase}).
	reg := prometheus.NewRegistry()
	promReader, err := promexporter.New(promexporter.WithRegisterer(reg), promexporter.WithoutScopeInfo())
	if err != nil {
		return nil, nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	metricOpts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithReader(promReader)}

	var tracer trace.TracerProvider = tracenoop.NewTracerProvider()
	var lp *sdklog.LoggerProvider
	shutdowns := []func(context.Context) error{}

	if cfg.Endpoint != "" {
		// Exporters dial lazily and retry in the background, so a collector that
		// is down never blocks startup.
		exp, err := newOTLPExporters(c, cfg)
		if err != nil {
			return nil, nil, err
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exp.trace))
		tracer = tp
		lp = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exp.log)))
		shutdowns = append(shutdowns, tp.Shutdown, lp.Shutdown)
		metricOpts = append(metricOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp.metric)))
	}

	mp := sdkmetric.NewMeterProvider(metricOpts...)
	shutdowns = append(shutdowns, mp.Shutdown)
	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, nil, fmt.Errorf("runtime metrics: %w", err)
	}

	var logger *slog.Logger
	if lp != nil {
		logger = newLogger(logCfg, lp)
		logger.Info("otel exporting", "endpoint", cfg.Endpoint, "service", serviceName(cfg))
	} else {
		logger = newLogger(logCfg, nil)
	}

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		var errs []error
		for _, s := range shutdowns {
			errs = append(errs, s(ctx))
		}
		if err := errors.Join(errs...); err != nil {
			logger.Warn("otel shutdown", "error", err)
		}
	}

	return &Providers{
		Tracer:         tracer,
		Meter:          mp,
		Propagator:     propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
		Logger:         logger,
		MetricsHandler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
	}, cleanup, nil
}

type exporters struct {
	trace  sdktrace.SpanExporter
	metric sdkmetric.Exporter
	log    sdklog.Exporter
}

func newOTLPExporters(c context.Context, cfg config.OTel) (*exporters, error) {
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	logOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}

	var exp exporters
	var err error
	if exp.trace, err = otlptracegrpc.New(c, traceOpts...); err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	if exp.metric, err = otlpmetricgrpc.New(c, metricOpts...); err != nil {
		_ = exp.trace.Shutdown(c)
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	if exp.log, err = otlploggrpc.New(c, logOpts...); err != nil {
		_ = exp.trace.Shutdown(c)
		_ = exp.metric.Shutdown(c)
		return nil, fmt.Errorf("otlp log exporter: %w", err)
	}

	return &exp, nil
}

// newResource sets service.name and service.instance.id explicitly: the OTLP →
// Prometheus path turns only those two into series labels (job / instance);
// without instance.id every replica writes the same series and counters jump.
func newResource(c context.Context, cfg config.OTel) (*resource.Resource, error) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}

	res, err := resource.New(c,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", serviceName(cfg)),
			attribute.String("service.instance.id", host+":"+strconv.Itoa(os.Getpid())),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	return res, nil
}

func serviceName(cfg config.OTel) string {
	if cfg.ServiceName != "" {
		return cfg.ServiceName
	}

	return filepath.Base(os.Args[0])
}
