// Package pagination is the transport-agnostic half of cursor (keyset) paging:
// page-size clamping and opaque page tokens. The sort key and the SQL belong to
// each bounded context; this package only hides a payload from clients.
//
// Tokens are base64url(JSON payload) with no padding. They are opaque, not
// secret: a client can decode one, but a tampered or stale token fails to
// decode or no longer matches the request's filters and is rejected as an
// invalid argument. Embed the filters in the payload so that check is possible.
package pagination

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/blackhorseya/x-monorepo/pkgs/kernel"
)

var (
	// ErrInvalidPageSize is returned for a negative page size.
	ErrInvalidPageSize = kernel.NewError(kernel.KindInvalidArgument, "pagination.invalid_page_size", "page size must not be negative")
	// ErrInvalidToken is returned for a token this server did not issue, or one
	// issued for different filters.
	ErrInvalidToken = kernel.NewError(kernel.KindInvalidArgument, "pagination.invalid_token", "page token is invalid")
)

// Clamp resolves the page size a client sent: 0 means def, anything above max
// is reduced to max, negative is an error.
func Clamp(size int32, def, max int) (int, error) {
	switch {
	case size < 0:
		return 0, ErrInvalidPageSize
	case size == 0:
		return def, nil
	case int(size) > max:
		return max, nil
	default:
		return int(size), nil
	}
}

// EncodeToken serialises payload into an opaque page token.
func EncodeToken(payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode page token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeToken parses token into payload. Unknown fields are rejected so a
// token from another listing cannot pass as this one; any failure is ErrInvalidToken.
func DecodeToken(token string, payload any) error {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return ErrInvalidToken
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(payload); err != nil {
		return ErrInvalidToken
	}

	return nil
}
