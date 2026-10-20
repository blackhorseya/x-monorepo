package admin

import "errors"

var (
	errNoRow     = errors.New("no such poison message")
	errNoDB      = errors.New("no database configured: poison tables live in Postgres")
	errUnknownBC = errors.New("unknown bounded context")
)
