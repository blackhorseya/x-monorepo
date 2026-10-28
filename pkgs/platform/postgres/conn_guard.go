package postgres

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

// ConnPolicy is what a caller demands of the database connection before it
// opens one: `worker bootstrap` always, any other app through
// postgres.expect_target.
type ConnPolicy struct {
	// ExpectTarget, when set, is the host:port every connection attempt must
	// dial (for example 127.0.0.1:15432 behind a kubectl port-forward). It
	// stops a host= (or a host list) in the URL from sending the connection
	// somewhere other than the forward.
	ExpectTarget string
	// AllowPlaintext skips the TLS check, for a local development database.
	// ExpectTarget still applies.
	AllowPlaintext bool
}

// CheckConn parses url the way the connection itself will and
// validates the result.
//
// The parse is pgx.ParseConfig, the one gorm's postgres driver runs on a DSN,
// so what is approved here is what pgx connects with: key trimming, duplicate keys (the last wins), URL decoding,
// host= overrides, environment and service-file settings are all already
// applied. A shell or a regexp reading the URL text cannot promise that.
//
// Errors say why and nothing parsed from the URL or the environment: not the
// host, port, user or database, and not pgx's own parse error, which can quote
// the string. A password with an unencoded @ makes pgx cut part of it out as
// the host, so even "which host" can be the secret. The only value a message
// carries is --expect-target, which the operator typed.
func CheckConn(url string, p ConnPolicy) error {
	_, err := ParseCheckedConn(url, p)

	return err
}

// ParseCheckedConn is CheckConn that hands back the config it approved. A
// caller that opens the connection must open it from this value, not from url
// again: the URL can name state outside itself (a service file, the
// environment) that changes between two parses, so a second parse is not
// guaranteed to be the one that was checked.
func ParseCheckedConn(url string, p ConnPolicy) (*pgx.ConnConfig, error) {
	// Before the URL is touched: without a target the operator typed there is
	// nothing to trust, because whatever pgx parses as the host may be a piece
	// of a malformed password (u:pw@SECRETLABEL looks like a perfectly good
	// host name).
	if p.ExpectTarget == "" {
		return nil, errors.New("--expect-target is required")
	}
	cc, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, errors.New("postgres.url is not a valid connection string")
	}
	if err := validateConn(&cc.Config, p); err != nil {
		return nil, err
	}

	return cc, nil
}

// validateConn checks a parsed config: every connection attempt —
// the main one and each fallback pgx will try next — must have a structurally
// valid host, dial the target the operator gave with --expect-target, and
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
func validateConn(cfg *pgconn.Config, p ConnPolicy) error {
	attempts := append([]*pgconn.Config{cfg}, fallbackConfigs(cfg)...)

	// Before anything else: a host that is not even shaped like one never
	// reaches the resolver, whose error quotes it. A second line of defence —
	// what actually keeps a piece of the password out of the log is the target
	// pin below, since a valid-looking label can be part of a password too.
	for _, a := range attempts {
		if !validHost(a.Host) {
			return errors.New("postgres.url has an invalid host")
		}
	}

	for _, a := range attempts {
		if net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port))) != p.ExpectTarget {
			return fmt.Errorf("connection target does not match --expect-target %s", p.ExpectTarget)
		}
	}
	if p.AllowPlaintext {
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
// It is a second line of defence only. pgx does not validate the host, so a
// password with an unencoded @ ("u:@SECRET@127.0.0.1") makes the host
// "SECRET@127.0.0.1", which the resolver quotes in its error and main logs;
// rejecting what cannot be a host name catches that early. It cannot catch a
// password piece that looks like a host name ("u:pw@SECRETLABEL/app"): no check
// on the URL alone can tell the two apart. That is what the --expect-target
// pin is for, and why the flag is required.
//
// One trailing dot (a fully qualified name, "localhost.") is allowed; an empty
// label anywhere else is not.
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
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
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
