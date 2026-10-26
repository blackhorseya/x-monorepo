package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// connPolicy is what `worker bootstrap` demands of the database connection
// before it opens one.
type connPolicy struct {
	// expectTarget, when set, is the host:port every connection attempt must
	// dial (for example 127.0.0.1:15432 behind a kubectl port-forward). It
	// stops a host= (or a host list) in the URL from sending the connection
	// somewhere other than the forward.
	expectTarget string
	// allowPlaintext skips the TLS check, for a local development database.
	// expectTarget still applies.
	allowPlaintext bool
}

// checkBootstrapConn parses url the way the connection itself will and
// validates the result.
//
// The parse is pgx.ParseConfig, the one gorm's postgres driver runs on the
// same string when it opens the pool, so what is approved here is what pgx
// connects with: key trimming, duplicate keys (the last wins), URL decoding,
// host= overrides, environment and service-file settings are all already
// applied. A shell or a regexp reading the URL text cannot promise that.
//
// Errors say why and, at most, which host:port. pgx's own parse error can
// quote the connection string, so it is not passed on.
func checkBootstrapConn(url string, p connPolicy) error {
	cc, err := pgx.ParseConfig(url)
	if err != nil {
		return errors.New("postgres.url is not a valid connection string")
	}

	return validateBootstrapConn(&cc.Config, p)
}

// validateBootstrapConn checks a parsed config: every connection attempt —
// the main one and each fallback pgx will try next — must dial the expected
// target, and unless plaintext is allowed must verify the server.
//
// What "verifies the server" looks like after pgx's configTLS
// (pgx v5.11.0, pgconn/config.go):
//   - sslmode=disable: TLSConfig is nil.
//   - allow, prefer: InsecureSkipVerify with no VerifyPeerCertificate, and
//     prefer/allow also leave a nil-TLS (plaintext) fallback attempt.
//   - require without sslrootcert: InsecureSkipVerify, no VerifyPeerCertificate
//     (encrypted, not authenticated). With sslrootcert pgx treats require as
//     verify-ca, as libpq does.
//   - verify-ca: InsecureSkipVerify (to skip the hostname check) but a
//     VerifyPeerCertificate that verifies the chain against sslrootcert.
//   - verify-full: InsecureSkipVerify false, ServerName set.
//
// So a TLSConfig is acceptable when it is non-nil and either verifies by
// itself (InsecureSkipVerify false) or has installed its own chain check.
func validateBootstrapConn(cfg *pgconn.Config, p connPolicy) error {
	type attempt struct {
		target   string
		verified bool
		tls      bool
	}
	attempts := []attempt{{
		target:   net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))),
		tls:      cfg.TLSConfig != nil,
		verified: cfg.TLSConfig != nil && (!cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.VerifyPeerCertificate != nil),
	}}
	for _, fb := range cfg.Fallbacks {
		attempts = append(attempts, attempt{
			target:   net.JoinHostPort(fb.Host, strconv.Itoa(int(fb.Port))),
			tls:      fb.TLSConfig != nil,
			verified: fb.TLSConfig != nil && (!fb.TLSConfig.InsecureSkipVerify || fb.TLSConfig.VerifyPeerCertificate != nil),
		})
	}

	if p.expectTarget != "" {
		for _, a := range attempts {
			if a.target != p.expectTarget {
				return fmt.Errorf("target mismatch: got %s, want %s", a.target, p.expectTarget)
			}
		}
	}
	if p.allowPlaintext {
		return nil
	}
	for _, a := range attempts {
		switch {
		case !a.tls:
			return fmt.Errorf("TLS disabled for %s: use sslmode=verify-ca with sslrootcert, or verify-full", a.target)
		case !a.verified:
			return fmt.Errorf("TLS does not verify the server certificate for %s: use sslmode=verify-ca with sslrootcert, or verify-full", a.target)
		}
	}

	return nil
}
