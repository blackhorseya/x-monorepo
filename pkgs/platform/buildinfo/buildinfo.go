// Package buildinfo carries what the release pipeline stamps into a binary.
// goreleaser sets the three variables with -X ldflags (see .goreleaser.yaml);
// a plain `go build` leaves the defaults, which is how a dev binary announces
// itself in telemetry (service.version=dev).
//
// Where it surfaces: OTel resource service.version, the "listening" / "worker
// started" log lines, `--version` on every binary and GET /api/version on rpc.
package buildinfo

import (
	"log/slog"
	"runtime"
)

var (
	// Version is the release tag without the "v" (0.1.0), or "dev".
	Version = "dev"
	// Commit is the full git SHA the binary was built from, or "none".
	Commit = "none"
	// Date is the commit date in RFC 3339, or "unknown".
	Date = "unknown"
)

// Info is the stamped build identity plus the Go runtime that built it; the
// JSON tags are the /api/version wire shape.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Get returns the identity of the running binary.
func Get() Info {
	return Info{Version: Version, Commit: Commit, Date: Date, Go: runtime.Version()}
}

// String is the `--version` line: "0.20.0 (ac56388…, 2026-10-23T05:18:59Z, go1.25.3)".
func (x Info) String() string {
	return x.Version + " (" + x.Commit + ", " + x.Date + ", " + x.Go + ")"
}

// LogAttrs is the same identity as slog attributes for a start-up line.
func (x Info) LogAttrs() []any {
	return []any{slog.String("version", x.Version), slog.String("commit", x.Commit), slog.String("build_date", x.Date)}
}
