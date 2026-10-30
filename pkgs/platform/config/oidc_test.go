package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

func loadYAML(t *testing.T, yaml string) (*config.AppConfig, error) {
	t.Helper()
	dir := chdirTemp(t)
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	return config.Load()
}

// OIDC is off by default and, off, nothing about it is checked: a deployment
// that never heard of it loads exactly as before.
func TestOIDCConfig_DisabledByDefaultAndUnchecked(t *testing.T) {
	chdirTemp(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.OIDC.Enabled || len(cfg.Auth.OIDC.Issuers) != 0 {
		t.Fatalf("oidc defaults: %+v", cfg.Auth.OIDC)
	}

	// Garbage issuers are ignored while enabled is false.
	if _, err := loadYAML(t, "auth:\n  oidc:\n    enabled: false\n    issuers:\n      - issuer: \"ftp://nope\"\n"); err != nil {
		t.Fatalf("disabled oidc must not be validated: %v", err)
	}
}

func TestOIDCConfig_Valid(t *testing.T) {
	cfg, err := loadYAML(t, `auth:
  oidc:
    enabled: true
    issuers:
      - issuer: https://idp.example.com
        audience: console
      - issuer: http://dex.localtest.me:5556/dex
        audience: console-dev
        mapping: auto_provision
        allow_insecure_issuer: true
      - issuer: http://127.0.0.1:5556/dex
        audience: console-loopback
`)
	if err != nil {
		t.Fatal(err)
	}
	is := cfg.Auth.OIDC.Issuers
	if len(is) != 3 || is[0].Audience != "console" || !is[1].AllowInsecureIssuer {
		t.Fatalf("issuers: %+v", is)
	}
	// mapping defaults to bound (fail-closed) and keeps an explicit value.
	if is[0].EffectiveMapping() != config.MappingBound || is[1].EffectiveMapping() != config.MappingAutoProvision {
		t.Fatalf("mapping: %q %q", is[0].EffectiveMapping(), is[1].EffectiveMapping())
	}
}

func TestOIDCConfig_Rejected(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no issuers":           {"auth:\n  oidc:\n    enabled: true\n", "issuer"},
		"empty issuer":         {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - audience: a\n", "issuer"},
		"empty audience":       {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - issuer: https://idp.example.com\n", "audience"},
		"duplicate issuer":     {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: https://idp.example.com, audience: a}\n      - {issuer: https://idp.example.com, audience: b}\n", "duplicate"},
		"unknown mapping":      {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: https://idp.example.com, audience: a, mapping: email}\n", "mapping"},
		"http non-loopback":    {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: \"http://idp.example.com\", audience: a}\n", "allow_insecure_issuer"},
		"not http(s)":          {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: \"ftp://idp.example.com\", audience: a, allow_insecure_issuer: true}\n", "scheme"},
		"no host":              {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: \"https://\", audience: a}\n", "host"},
		"issuer with query":    {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: \"https://idp.example.com?x=1\", audience: a}\n", "query"},
		"issuer with fragment": {"auth:\n  oidc:\n    enabled: true\n    issuers:\n      - {issuer: \"https://idp.example.com#x\", audience: a}\n", "fragment"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// docs/identity-login.md §3: with OIDC on and the legacy auth.jwt configured,
// auth.jwt.issuer must be set and differ from every OIDC issuer — otherwise a
// wildcard legacy verifier overlaps the OIDC trust boundary.
func TestOIDCConfig_LegacyJWTIssuerPin(t *testing.T) {
	oidc := "  oidc:\n    enabled: true\n    issuers:\n      - {issuer: https://idp.example.com, audience: a}\n"
	for name, tc := range map[string]struct {
		yaml    string
		wantErr string
	}{
		"legacy public key without issuer":   {"auth:\n  jwt:\n    public_key: \"pem\"\n" + oidc, "auth.jwt.issuer"},
		"legacy secret without issuer":       {"auth:\n  jwt:\n    hs256_secret: \"0123456789abcdef0123456789abcdef\"\n" + oidc, "auth.jwt.issuer"},
		"legacy issuer equals oidc issuer":   {"auth:\n  jwt:\n    hs256_secret: \"0123456789abcdef0123456789abcdef\"\n    issuer: https://idp.example.com\n" + oidc, "auth.jwt.issuer"},
		"legacy issuer pinned and different": {"auth:\n  jwt:\n    hs256_secret: \"0123456789abcdef0123456789abcdef\"\n    issuer: https://legacy.example.com\n" + oidc, ""},
		"no legacy verifier":                 {"auth:\n" + oidc, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	// The same legacy config without OIDC is untouched (enabled=false changes nothing).
	if _, err := loadYAML(t, "auth:\n  jwt:\n    hs256_secret: \"0123456789abcdef0123456789abcdef\"\n"); err != nil {
		t.Fatalf("legacy jwt without issuer must still load while oidc is off: %v", err)
	}
}

func TestCheckIssuerURL(t *testing.T) {
	for _, tc := range []struct {
		issuer   string
		insecure bool
		ok       bool
	}{
		{"https://idp.example.com", false, true},
		{"https://idp.example.com/realms/x", false, true},
		{"http://localhost:5556/dex", false, true},
		{"http://127.0.0.1:5556", false, true},
		{"http://127.1.2.3", false, true},
		{"http://[::1]:5556", false, true},
		{"http://dex.localtest.me:5556/dex", false, false},
		{"http://dex.localtest.me:5556/dex", true, true},
		{"http://idp.example.com", false, false},
		{"http://127.0.0.1.evil.example", false, false}, // loopback is the host, not a prefix of it
		{"http://localhost.evil.example", false, false},
		{"ftp://idp.example.com", true, false},
		{"", true, false},
		{"idp.example.com", true, false},
	} {
		err := config.CheckIssuerURL(tc.issuer, tc.insecure)
		if (err == nil) != tc.ok {
			t.Errorf("CheckIssuerURL(%q, insecure=%v) = %v, want ok=%v", tc.issuer, tc.insecure, err, tc.ok)
		}
	}
}
