package acceptance

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/server/servertest"
)

// ExpectRejected asserts what a call was actually answered: a Connect error with the given code (its String(), e.g.
// "unauthenticated") and x-error-code, not an HTTP status. The client that made the call must carry
// servertest.WithErrorMeta, or the x-error-code cannot be read back.
func ExpectRejected(err error, code, errorCode string) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return fmt.Errorf("want a connect error %s / %s, got %w", code, errorCode, err)
	}
	if got := ce.Code().String(); got != code {
		return fmt.Errorf("connect code = %q, want %q: %w", got, code, err)
	}
	if got := servertest.ErrorMeta(err).Get("x-error-code"); got != errorCode {
		return fmt.Errorf("x-error-code = %q, want %q: %w", got, errorCode, err)
	}

	return nil
}

// QueryText runs query on the scenario's own database and returns every row with each column as text (cast in the SQL
// what is not text, e.g. encode(payload, 'hex')). The connection lives for this call only, so nothing stays connected to a
// database the runner is about to drop. A step uses it to observe what the product wrote (an outbox row, an inbox row).
//
// The database URL carries the password: no error from here contains it. A failed connect says so in a fixed sentence.
func (e *Env) QueryText(c context.Context, query string, args ...any) ([][]string, error) {
	conn, err := pgx.Connect(c, e.DatabaseURL)
	if err != nil {
		return nil, errors.New("cannot connect to the scenario database") // err names the host and user; fixed text, never the url
	}
	defer func() { _ = conn.Close(context.WithoutCancel(c)) }()

	rows, err := conn.Query(c, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query the scenario database: %w", err)
	}
	defer rows.Close()

	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("read a row of the scenario database: %w", err)
		}
		row := make([]string, len(vals))
		for i, v := range vals {
			if v != nil {
				row[i] = fmt.Sprint(v)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the scenario database: %w", err)
	}

	return out, nil
}
