package postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// connError is what a failed connection attempt becomes before it leaves this
// package: a category and, for a server rejection, its SQLSTATE. Nothing else.
//
// pgx's own text puts the user and database in the message, the server's
// message can name the role or echo a setting value, and either can quote a
// piece of the connection string; a password with an unencoded @ or one that
// looks like a setting makes even "which host" a secret. So the message is
// built here from fixed words, and only the operator's own expect_target (a
// value they typed, and one the connection was checked against) may appear in
// it. A host parsed from the URL is never printed: pgx does not validate it, a
// password with an unencoded @ has part of itself cut out as the host, and a
// piece of a password can look exactly like a host name, so no check on the
// parsed value is enough. That is what expect_target is for.
//
// The category stays checkable: errors.Is answers for the causes in the original
// chain (context.DeadlineExceeded, syscall.ECONNREFUSED and the like), and
// errors.As to *pgconn.PgError yields a copy that has the Code and Severity but
// no Message, Detail or other server text. No other type of the original chain
// is reachable from here.
type connError struct {
	kind     string
	sqlstate string
	severity string
	cause    error
}

func (e *connError) Error() string {
	if e.sqlstate != "" {
		return fmt.Sprintf("%s (SQLSTATE %s)", e.kind, e.sqlstate)
	}

	return e.kind
}

func (e *connError) Is(target error) bool { return errors.Is(e.cause, target) }

func (e *connError) As(target any) bool {
	if pp, ok := target.(**pgconn.PgError); ok && e.sqlstate != "" {
		*pp = &pgconn.PgError{Severity: e.severity, Code: e.sqlstate}

		return true
	}

	return false
}

// redactConnErr reduces err, from opening or pinging a connection, to a
// connError. target is the operator's postgres.expect_target, or "".
func redactConnErr(err error, target string) error {
	if err == nil {
		return nil
	}
	e := &connError{cause: err}
	var (
		pgErr *pgconn.PgError
		dns   *net.DNSError
		unk   x509.UnknownAuthorityError
		cert  *tls.CertificateVerificationError
		rec   tls.RecordHeaderError
	)
	switch {
	case errors.As(err, &pgErr):
		e.kind = "server rejected the connection"
		// The server is not trusted for text: a code that is not a SQLSTATE or
		// a severity that is not one of Postgres's own is dropped, and with no
		// valid code there is no PgError to hand out.
		if validSQLSTATE(pgErr.Code) {
			e.sqlstate = pgErr.Code
			if severities[pgErr.Severity] {
				e.severity = pgErr.Severity
			}
		}
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		e.kind = "connection timed out"
	case errors.As(err, &dns):
		e.kind = "host lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		e.kind = "connection refused"
	case errors.As(err, &cert) || errors.As(err, &unk) || errors.As(err, &rec):
		e.kind = "TLS handshake failed"
	default:
		e.kind = "connection failed"
	}
	if target != "" {
		e.kind += " to " + target
	}

	return e
}

func isTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// redactIfConnect redacts err when it is a failure to establish a connection,
// or a URL that did not parse, and returns it untouched otherwise. migrate
// needs the split: a connect failure quotes the URL's user and database and the
// server's login message, and a parse failure quotes the URL, while an error
// from a migration's own SQL, after the connection is up, is exactly what the
// operator has to read to fix it.
func redactIfConnect(err error, target string) error {
	var (
		parse *pgconn.ParseConfigError
		conn  *pgconn.ConnectError
	)
	switch {
	case errors.As(err, &parse):
		return errUnparsableURL
	case errors.As(err, &conn):
		return redactConnErr(err, target)
	}

	return err
}

// errUnparsableURL is what a URL pgx cannot parse is reported as: pgx's own
// error quotes it, user and database included.
var errUnparsableURL = errors.New("postgres.url is not a valid connection string")

// validSQLSTATE reports whether c is a SQLSTATE: five characters of 0-9 and A-Z.
func validSQLSTATE(c string) bool {
	if len(c) != 5 {
		return false
	}
	for i := 0; i < len(c); i++ {
		if ch := c[i]; (ch < '0' || ch > '9') && (ch < 'A' || ch > 'Z') {
			return false
		}
	}

	return true
}

// severities are the severity strings Postgres itself sends in an error or
// notice (the S field, which can be localized, is not among them when it is).
var severities = map[string]bool{
	"ERROR": true, "FATAL": true, "PANIC": true, "WARNING": true,
	"NOTICE": true, "DEBUG": true, "INFO": true, "LOG": true,
}
