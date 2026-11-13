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

// parseCheckedConn parses url the way the connection itself will and checks
// the result against target, the host:port of postgres.expect_target. Callers
// run it only when target is set; an empty target matches no host:port, so it
// would reject every URL rather than pass one.
//
// The parse is pgx.ParseConfig, the one gorm's postgres driver runs on a DSN,
// so what is approved here is what pgx connects with: key trimming, duplicate
// keys (the last wins), URL decoding, host= overrides, environment and
// service-file settings are all already applied. A shell or a regexp reading
// the URL text cannot promise that. The caller opens the connection from the
// returned config, not from url again: the URL can name state outside itself (a
// service file, the environment) that changes between two parses, so a second
// parse is not guaranteed to be the one that was checked.
//
// Errors say why and nothing parsed from the URL or the environment: not the
// host, port, user or database, and not pgx's own parse error, which can quote
// the string. A password with an unencoded @ makes pgx cut part of it out as
// the host, so even "which host" can be the secret. The only value a message
// carries is target, which the operator configured.
func parseCheckedConn(url, target string) (*pgx.ConnConfig, error) {
	cc, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, errors.New("postgres.url is not a valid connection string")
	}
	if err := validateConn(&cc.Config, target); err != nil {
		return nil, err
	}

	return cc, nil
}

// validateConn checks a parsed config: every connection attempt — the main one
// and each fallback pgx will try next — must have a structurally valid host and
// dial target.
func validateConn(cfg *pgconn.Config, target string) error {
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
		if net.JoinHostPort(a.Host, strconv.Itoa(int(a.Port))) != target {
			return fmt.Errorf("connection target does not match %s", target)
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
// on the URL alone can tell the two apart. That is what the target pin in
// validateConn is for.
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
