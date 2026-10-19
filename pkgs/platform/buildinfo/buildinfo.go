// Package buildinfo carries what the release pipeline stamps into a binary.
// goreleaser sets the three variables with -X ldflags (see .goreleaser.yaml);
// a plain `go build` leaves the defaults, which is how a dev binary announces
// itself in telemetry (service.version=dev).
package buildinfo

var (
	// Version is the release tag without the "v" (0.1.0), or "dev".
	Version = "dev"
	// Commit is the full git SHA the binary was built from, or "none".
	Commit = "none"
	// Date is the commit date in RFC 3339, or "unknown".
	Date = "unknown"
)
