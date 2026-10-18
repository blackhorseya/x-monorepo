package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// Probe paths. Liveness answers "is the process alive" and never looks at
// dependencies: restarting a pod because Postgres is down helps nobody.
// Readiness answers "may traffic come here" and fails when a dependency check
// fails or the server is draining, so the load balancer stops routing first.
const (
	HealthzPath = "/api/healthz" // liveness
	ReadyzPath  = "/api/readyz"  // readiness
)

// Check is one readiness dependency: Probe returns nil when it can serve.
// Apps assemble the list (Readiness) from what they are wired with.
type Check struct {
	Name  string
	Probe func(c context.Context) error
}

// Readiness is the app's dependency checks, a named type so wire can provide it.
type Readiness []Check

// checkTimeout bounds every probe so a hung dependency reports "not ready"
// instead of hanging the probe itself.
const checkTimeout = 2 * time.Second

// health owns the probe handlers and the draining flag Run flips at shutdown.
type health struct {
	checks   Readiness
	draining atomic.Bool
}

func (h *health) live(c *gin.Context) { c.Status(http.StatusOK) }

// ready runs every check and reports each by name: 200 when all pass,
// 503 with the failing ones' errors otherwise (or "draining" at shutdown).
func (h *health) ready(c *gin.Context) {
	status := http.StatusOK
	report := make(map[string]string, len(h.checks)+1)
	if h.draining.Load() {
		status = http.StatusServiceUnavailable
		report["server"] = "draining"
	}
	for _, chk := range h.checks {
		ctx, cancel := context.WithTimeout(c.Request.Context(), checkTimeout)
		err := chk.Probe(ctx)
		cancel()
		if err != nil {
			status = http.StatusServiceUnavailable
			report[chk.Name] = err.Error()
			continue
		}
		report[chk.Name] = "ok"
	}
	c.JSON(status, report)
}
