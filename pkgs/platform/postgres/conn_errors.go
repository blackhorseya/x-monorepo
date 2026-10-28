package postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// connError is what a failed connection attempt becomes before it leaves this
// package: a category and, for a server rejection, its SQLSTATE. Nothing else.
//
// pgx's own text puts the user and database in the message, the server's
// message can name the role or echo a setting value, and either can quote a
// piece of the connection string; a password with an unencoded @ or one that
// looks like a setting makes even "which host" a secret. So the message is
// built here from fixed words plus the host:port of each target the config
// tried, which is what an operator needs to find the failing side. Those come
// from the parsed config and only when every one of them is shaped like a host
// (validHost): pgx does not validate it, and a password with an unencoded @ has
// part of itself cut out as the host. A host that is not shaped like one means
// no target is printed at all. A password fragment that merely looks like a
// host name cannot be told apart; that is what expect_target pins.
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
// connError. cc is the config the connection was made from.
func redactConnErr(err error, cc *pgx.ConnConfig) error {
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
		e.kind, e.sqlstate, e.severity = "server rejected the connection", pgErr.Code, pgErr.Severity
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
	if targets := connTargets(cc); targets != "" {
		e.kind += " to " + targets
	}

	return e
}

func isTimeout(err error) bool {
	var ne net.Error

	return errors.As(err, &ne) && ne.Timeout()
}

// connTargets lists the host:port of the main attempt and each fallback, or ""
// when there is no config or any host is not shaped like one.
func connTargets(cc *pgx.ConnConfig) string {
	if cc == nil {
		return ""
	}
	attempts := append([]*pgconn.Config{&cc.Config}, fallbackConfigs(&cc.Config)...)
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		if !validHost(a.Host) {
			return ""
		}
		out = append(out, net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port))))
	}

	return strings.Join(out, ", ")
}

// redactIfConnect redacts err when it is a failure to establish a connection
// and returns it untouched otherwise. migrate needs the split: a connect
// failure quotes the URL's user and database and the server's login message,
// while an error from a migration's own SQL, after the connection is up, is
// exactly what the operator has to read to fix it.
func redactIfConnect(err error, cc *pgx.ConnConfig) error {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return redactConnErr(err, cc)
	}

	return err
}
