package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

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
// Errors say why and nothing parsed from the URL or the environment: not the
// host, port, user or database, and not pgx's own parse error, which can quote
// the string. A password with an unencoded @ makes pgx cut part of it out as
// the host, so even "which host" can be the secret. The only value a message
// carries is --expect-target, which the operator typed.
func checkBootstrapConn(url string, p connPolicy) error {
	cc, err := pgx.ParseConfig(url)
	if err != nil {
		return errors.New("postgres.url is not a valid connection string")
	}

	return validateBootstrapConn(&cc.Config, p)
}

// validateBootstrapConn checks a parsed config: every connection attempt —
// the main one and each fallback pgx will try next — must have a structurally
// valid host (always), dial the expected target (when one is given), and
// unless plaintext is allowed must verify the server.
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
	attempts := append([]*pgconn.Config{cfg}, fallbackConfigs(cfg)...)

	// Before anything else, and with or without --expect-target: a host that
	// is not a host never reaches the resolver, whose error quotes it.
	for _, a := range attempts {
		if !validHost(a.Host) {
			return errors.New("postgres.url has an invalid host")
		}
	}

	if p.expectTarget != "" {
		for _, a := range attempts {
			if net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port))) != p.expectTarget {
				return fmt.Errorf("connection target does not match --expect-target %s", p.expectTarget)
			}
		}
	}
	if p.allowPlaintext {
		return nil
	}
	for _, a := range attempts {
		switch {
		case a.TLSConfig == nil:
			return errors.New("TLS is disabled for the connection: use sslmode=verify-ca with sslrootcert, or verify-full")
		case a.TLSConfig.InsecureSkipVerify && a.TLSConfig.VerifyPeerCertificate == nil:
			return errors.New("TLS does not verify the server certificate: use sslmode=verify-ca with sslrootcert, or verify-full")
		}
	}

	return nil
}

// fallbackConfigs lifts each fallback attempt to a Config so it is checked
// the same way as the main one.
func fallbackConfigs(cfg *pgconn.Config) []*pgconn.Config {
	out := make([]*pgconn.Config, 0, len(cfg.Fallbacks))
	for _, fb := range cfg.Fallbacks {
		out = append(out, &pgconn.Config{Host: fb.Host, Port: fb.Port, TLSConfig: fb.TLSConfig})
	}

	return out
}

// validHost reports whether h is something pgx can dial: an IP address, a DNS
// name (RFC 1123 labels: 1-63 letters, digits or hyphens, not starting or
// ending with a hyphen, 253 bytes at most), or a unix socket directory.
//
// It exists because pgx does not validate the host. A password with an
// unencoded @ ("u:@SECRET@127.0.0.1") makes the host "SECRET@127.0.0.1", which
// the resolver then quotes in its error ("lookup SECRET@127.0.0.1"), and main
// logs that. Nothing that could carry a secret is a valid host: no @, %, space
// or other punctuation.
func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if strings.HasPrefix(h, "/") {
		// A socket directory (PGHOST=/var/run/postgresql, host=/tmp): a plain
		// path, no spaces or other characters a secret would need.
		return !strings.ContainsFunc(h, func(r rune) bool { return !isHostRune(r) && r != '/' && r != '.' && r != '_' })
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !isHostRune(r) {
				return false
			}
		}
	}

	return true
}

// isHostRune is a letter, a digit or a hyphen: what a DNS label is made of.
func isHostRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}
