package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/server"
)

// defaultWaitBudget is how long `rpc --wait-catalog` waits when no duration is given.
const defaultWaitBudget = 60 * time.Second

// cliOptions is what the command line asks for. There are two things a command line can ask of rpc, and
// they exclude each other: its version, or the wait an initContainer runs before the server starts.
type cliOptions struct {
	version     bool
	waitCatalog bool
	waitBudget  time.Duration
}

// parseArgs reads the command line (without the program name). Anything it does not know is an error: a
// typo must not start the server, or skip the wait, quietly.
func parseArgs(args []string) (cliOptions, error) {
	var o cliOptions
	if len(args) == 0 {
		return o, nil
	}
	if len(args) > 1 {
		return cliOptions{}, errors.New("rpc: at most one argument (--version or --wait-catalog[=duration])")
	}
	switch a := args[0]; {
	case a == "--version" || a == "-v":
		o.version = true
	case a == "--wait-catalog":
		o.waitCatalog, o.waitBudget = true, defaultWaitBudget
	case strings.HasPrefix(a, "--wait-catalog="):
		d, err := time.ParseDuration(strings.TrimPrefix(a, "--wait-catalog="))
		if err != nil || d <= 0 {
			return cliOptions{}, errors.New("rpc: --wait-catalog=<duration> wants a positive duration such as 60s")
		}
		o.waitCatalog, o.waitBudget = true, d
	default:
		return cliOptions{}, errors.New("rpc: unknown argument (the only ones are --version and --wait-catalog[=duration])")
	}

	return o, nil
}

// waitForCatalog is the best-effort wait of an initContainer: it probes the catalog's /api/readyz (no
// credential: the probe path is open) every `every` until it answers 200 or `budget` is spent, then
// returns. A catalog that does not come in time is a WARN, not an error: a new rpc pod must start when
// the catalog is down (rpc's own readyz does not depend on it; CreateOrder answers UNAVAILABLE until the
// catalog is back), so the wait can order a rollout in the normal case and never block one in the bad
// case. The log names the upstream URL and never the service key.
func waitForCatalog(c context.Context, cfg config.Catalog, budget, every time.Duration, logger *slog.Logger) error {
	if u, err := url.Parse(cfg.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("rpc --wait-catalog: catalog.upstream must be an http(s) URL")
	}
	c, cancel := context.WithTimeout(c, budget)
	defer cancel()

	readyz := strings.TrimRight(cfg.Upstream, "/") + server.ReadyzPath
	last := "no answer yet"
	for {
		req, err := http.NewRequestWithContext(c, http.MethodGet, readyz, nil)
		if err == nil {
			res, err := http.DefaultClient.Do(req)
			switch {
			case err != nil:
				last = "unreachable"
			default:
				_ = res.Body.Close()
				if res.StatusCode == http.StatusOK {
					return nil
				}
				last = fmt.Sprintf("readyz answered %d", res.StatusCode)
			}
		}
		select {
		case <-c.Done():
			if errors.Is(c.Err(), context.DeadlineExceeded) {
				logger.Warn("catalog is not ready, starting anyway: CreateOrder and SetStock answer UNAVAILABLE until it is",
					"upstream", cfg.Upstream, "waited", budget.String(), "last", last)
			}

			return nil
		case <-time.After(every):
		}
	}
}
