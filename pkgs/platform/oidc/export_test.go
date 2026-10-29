package oidc

import "time"

// Test hooks: the production intervals are fixed (1 h refresh, 30 s unknown-kid
// window), the tests shrink them to run in real time.
var (
	DefaultRefreshInterval    = defaultRefreshInterval
	DefaultUnknownKIDInterval = unknownKIDInterval
	MaxStale                  = maxStale
)

func WithRefreshInterval(d time.Duration) Option { return func(o *options) { o.refreshInterval = d } }

func WithUnknownKIDInterval(d time.Duration) Option {
	return func(o *options) { o.unknownInterval = d }
}
