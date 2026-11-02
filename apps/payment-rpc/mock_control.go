package main

import (
	"errors"
	"io"
)

// runMock is `payment-rpc mock pay|notify|fail` (docs/payment.md §9 S6). Scaffold: the behaviour follows
// the RED tests.
func runMock(_ []string, _ io.Writer) error {
	return errors.New("payment-rpc mock: not implemented")
}
