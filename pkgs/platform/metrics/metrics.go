// Package metrics owns the process-wide Prometheus registry. Apps wire
// ProviderSet; components ask for prometheus.Registerer (to register) or
// prometheus.Gatherer (to expose) and never touch the global default registry,
// so every Initialize() call — including tests — gets an isolated registry.
package metrics

import (
	"github.com/google/wire"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// ProviderSet provides one registry per process, bound to both interfaces.
var ProviderSet = wire.NewSet(
	NewRegistry,
	wire.Bind(new(prometheus.Registerer), new(*prometheus.Registry)),
	wire.Bind(new(prometheus.Gatherer), new(*prometheus.Registry)),
)

// NewRegistry builds a registry with the standard Go runtime and process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return reg
}
