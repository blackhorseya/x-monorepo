package mock

import (
	"errors"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
)

// A correctly signed body that is not an event is refused as malformed, not as a bad signature: the
// signature is fine, the sender is not.
func TestVerifyWebhook_SignedButMalformed(t *testing.T) {
	p := New(Options{})
	for _, body := range []string{
		"not json",
		`{"id":"e","type":"t","ref":"r","amount":-1,"currency":"TWD"}`,
		`{"id":"e","type":"t","ref":"r","amount":1,"currency":"twd"}`,
	} {
		_, err := p.VerifyWebhook([]byte(body), p.sign([]byte(body)))
		if err == nil || errors.Is(err, port.ErrInvalidSignature) {
			t.Fatalf("body %q: err = %v, want a malformed-body error", body, err)
		}
	}
}
