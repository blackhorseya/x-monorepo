package acceptance

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// DBFailure is the error a step or the runner reports when a database call fails: a fixed sentence for what was being done and,
// when the server answered, its SQLSTATE. It never wraps err and never prints it. The server's message echoes the parameters of
// the statement and the driver's error names the host, the user and the query; a database URL (with its password) is a
// parameter a step can pass, so what the driver says is not text to trust, and a redaction of it would be a guess.
//
// A cancelled or expired c stays identifiable (errors.Is context.Canceled / context.DeadlineExceeded), so a wait that ends
// by its bound is still a timeout, not a database failure.
func DBFailure(c context.Context, what string, err error) error {
	if cerr := c.Err(); cerr != nil {
		return fmt.Errorf("%s: %w", what, cerr)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: the database answered with an error (SQLSTATE %s)", what, pe.Code)
	}

	return fmt.Errorf("%s: the database call failed (no SQLSTATE; the driver's text is not shown, it can hold the url)", what)
}

// QueryText runs query on the scenario's own database and returns every row with each column as text (cast in the SQL
// what is not text, e.g. encode(payload, 'hex')). The connection lives for this call only, so nothing stays connected to a
// database the runner is about to drop. A step uses it to observe what the product wrote (an outbox row, an inbox row).
//
// The database URL carries the password, and a server error echoes the statement's parameters: every error from here is
// DBFailure's, a fixed sentence with the SQLSTATE and nothing the driver or the server said.
func (e *Env) QueryText(c context.Context, query string, args ...any) ([][]string, error) {
	conn, err := pgx.Connect(c, e.DatabaseURL)
	if err != nil {
		return nil, DBFailure(c, "connect to the scenario database", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(c)) }()

	rows, err := conn.Query(c, query, args...)
	if err != nil {
		return nil, DBFailure(c, "query the scenario database", err)
	}
	defer rows.Close()

	var out [][]string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, DBFailure(c, "read a row of the scenario database", err)
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
		return nil, DBFailure(c, "read the scenario database", err)
	}

	return out, nil
}
