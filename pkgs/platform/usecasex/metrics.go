package usecasex

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// Recorder owns the use-case histogram. Build one per process from the
// MeterProvider (see platform/otel) and share it across every BC's decorators.
type Recorder struct {
	duration metric.Float64Histogram
}

// durationBuckets mirror prometheus.DefBuckets: OTel's defaults are tuned for
// milliseconds and would put every sub-second use case in the first bucket.
var durationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// NewRecorder creates usecase.duration (Prometheus: usecase_duration_seconds{usecase, kind}).
func NewRecorder(mp metric.MeterProvider) (*Recorder, error) {
	duration, err := mp.Meter("github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex").Float64Histogram(
		"usecase.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Use-case execution time by outcome kind (ok or kernel error kind)."),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	if err != nil {
		return nil, err
	}

	return &Recorder{duration: duration}, nil
}

// Metrics observes one histogram sample per execution. kind is "ok" on success,
// the kernel error kind for domain errors, and "internal" for anything else —
// the same split server.ToConnectError applies, so the attribute set stays finite.
func Metrics[I, O any](rec *Recorder, name string) Decorator[I, O] {
	return func(next UseCase[I, O]) UseCase[I, O] {
		return Func[I, O](func(c context.Context, in I) (O, error) {
			start := time.Now()

			out, err := next.Execute(c, in)

			rec.duration.Record(c, time.Since(start).Seconds(),
				metric.WithAttributes(attribute.String("usecase", name), attribute.String("kind", kindOf(err))))

			return out, err
		})
	}
}

func kindOf(err error) string {
	if err == nil {
		return "ok"
	}

	if errors.Is(err, kernel.ErrConcurrentUpdate) {
		return "aborted"
	}
	var de *kernel.Error
	if errors.As(err, &de) {
		return de.Kind().String()
	}

	return kernel.KindInternal.String()
}
