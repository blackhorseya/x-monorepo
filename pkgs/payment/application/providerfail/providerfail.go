// Package providerfail is the fixed vocabulary for WHY a payment provider call failed (#416).
//
// port.ErrProviderUnavailable says only that the provider gave no usable answer, and its text is fixed on
// purpose: a provider's own error is untrusted and may reflect a credential (#347). That left a stalled
// StartPayment looking like any other failure. A provider that knows the cause returns Unavailable(class)
// instead of the bare sentinel; the use case logs the class, which is one of three words, and nothing the
// provider or the driver said. It sits beside the use cases (like ownership) because application/port
// declares interfaces only.
package providerfail

import (
	"context"
	"errors"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
)

// Class is the FIXED category of a provider failure: the only thing about it that may be logged. It is a
// closed set; no value is ever built from text a provider produced.
type Class string

const (
	// Deadline: the caller's deadline ended while the call was still waiting (on the provider, or on the
	// store behind the mock).
	Deadline Class = "deadline"
	// Fault: the provider was told to fail (an injected fault, `payment-rpc mock fail`).
	Fault Class = "fault"
	// Other: anything else.
	Other Class = "other"
)

// failure is port.ErrProviderUnavailable that remembers its class. Its text is exactly the sentinel's and
// it is the sentinel for errors.Is, so every caller that matches the sentinel is unchanged.
type failure struct{ class Class }

func (failure) Error() string { return port.ErrProviderUnavailable.Error() }

func (failure) Is(target error) bool { return target == port.ErrProviderUnavailable }

// Unavailable is port.ErrProviderUnavailable tagged with why. It never takes the provider's own error.
func Unavailable(class Class) error { return failure{class: class} }

// Classify names the class of a failed provider call. A provider's own tag wins; an untagged error is a
// Deadline when the caller's context (or the error) says so, and Other otherwise. The text of err is never read.
func Classify(c context.Context, err error) Class {
	var f failure
	if errors.As(err, &f) {
		return f.class
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(c.Err(), context.DeadlineExceeded) {
		return Deadline
	}

	return Other
}
