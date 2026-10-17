package usecasex

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

// Recorder owns the use-case histogram. Build one per process from the
// registry (see platform/metrics) and share it across every BC's decorators.
type Recorder struct {
	duration *prometheus.HistogramVec
}

// NewRecorder registers usecase_duration_seconds{usecase, kind} on reg.
func NewRecorder(reg prometheus.Registerer) (*Recorder, error) {
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "usecase_duration_seconds",
		Help:    "Use-case execution time by outcome kind (ok or kernel error kind).",
		Buckets: prometheus.DefBuckets,
	}, []string{"usecase", "kind"})
	if err := reg.Register(duration); err != nil {
		return nil, err
	}

	return &Recorder{duration: duration}, nil
}

// Metrics observes one histogram sample per execution. kind is "ok" on success,
// the kernel error kind for domain errors, and "internal" for anything else —
// the same split server.ToConnectError applies, so the label set stays finite.
func Metrics[I, O any](rec *Recorder, name string) Decorator[I, O] {
	return func(next UseCase[I, O]) UseCase[I, O] {
		return Func[I, O](func(c context.Context, in I) (O, error) {
			start := time.Now()

			out, err := next.Execute(c, in)

			rec.duration.WithLabelValues(name, kindOf(err)).Observe(time.Since(start).Seconds())

			return out, err
		})
	}
}

func kindOf(err error) string {
	if err == nil {
		return "ok"
	}

	var de *kernel.Error
	if errors.As(err, &de) {
		return de.Kind().String()
	}

	return kernel.KindInternal.String()
}
