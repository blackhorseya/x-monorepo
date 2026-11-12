package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// oidcOn is the one config file a cell needs: auth.oidc.issuers is a list, and lists only come from a file.
const oidcOn = `auth:
  oidc:
    enabled: true
    issuers:
      - issuer: https://idp.example.invalid
        audience: client-x
`

// boot is the binary's startup path: config.Load (the APP_* env and an optional file, validated), then Initialize.
// It reports what a deployer sees: the process either starts (nil) or refuses with this error.
func boot(t *testing.T, mode string, oidc bool, cred string) error {
	t.Helper()
	t.Setenv("APP_HTTP_ADDR", ":0")
	t.Setenv("APP_LOG_LEVEL", "error")
	t.Setenv("APP_POSTGRES_URL", "")
	t.Setenv("APP_AUTH_MODE", mode)
	t.Setenv("APP_AUTH_API_KEYS", "")
	t.Setenv("APP_AUTH_JWT_HS256_SECRET", "")
	switch cred {
	case "api_keys":
		t.Setenv("APP_AUTH_API_KEYS", "adm=0123456789abcdefAAAA:admin")
	case "jwt":
		t.Setenv("APP_AUTH_JWT_HS256_SECRET", "0123456789abcdef0123456789abcdef01")
	}
	file := ""
	if oidc {
		file = filepath.Join(t.TempDir(), "oidc.yaml")
		if err := os.WriteFile(file, []byte(oidcOn), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("APP_CONFIG", file)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	_, cleanup, err := Initialize(cfg)
	if err != nil {
		return err
	}
	cleanup()

	return nil
}

// The cells where the template's rpc refuses to start. It wires no CredentialResolver and no
// ExternalIdentityResolver, so a credential has to come from the config (auth.api_keys, auth.jwt), and
// auth.oidc.enabled has nothing to map a verified identity to an account with.
func TestStartup_Refuses(t *testing.T) {
	const extension = "server.ExternalIdentityResolver"
	cells := []struct {
		name, mode string
		oidc       bool
		cred       string
		msg        string // what the error must contain ("" = any refusal)
	}{
		// auth.oidc.enabled with no resolver is refused whatever auth.mode says, and the error names the extension point.
		{"oidc on, mode off, no credential", "off", true, "none", extension},
		{"oidc on, mode off, api_keys", "off", true, "api_keys", extension},
		{"oidc on, mode optional, no credential", "optional", true, "none", extension},
		{"oidc on, mode optional, api_keys", "optional", true, "api_keys", extension},
		{"oidc on, mode required, no credential", "required", true, "none", extension},
		{"oidc on, mode required, api_keys", "required", true, "api_keys", extension},
		// auth on with nothing to check a credential against: nobody could ever get in.
		{"mode optional, no credential", "optional", false, "none", ""},
		{"mode required, no credential", "required", false, "none", ""},
	}
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			err := boot(t, c.mode, c.oidc, c.cred)
			if err == nil {
				t.Fatalf("mode=%s oidc=%v credential=%s: started, want a refusal", c.mode, c.oidc, c.cred)
			}
			if !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("refusal = %q, want it to contain %q", err, c.msg)
			}
		})
	}
}

// The cells where it starts: no auth, or auth with a credential the config itself holds (a static key or a JWT).
// The deployment shapes in the repo (configs/config.example.yaml, the chart's values and ci profiles, the Console
// e2e servers) all land in one of these.
func TestStartup_Starts(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		cred       string
	}{
		{"mode off, no credential", "off", "none"},
		{"mode off, api_keys", "off", "api_keys"},
		{"mode off, jwt", "off", "jwt"},
		{"mode optional, api_keys", "optional", "api_keys"},
		{"mode optional, jwt", "optional", "jwt"},
		{"mode required, api_keys", "required", "api_keys"},
		{"mode required, jwt", "required", "jwt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := boot(t, c.mode, false, c.cred); err != nil {
				t.Fatalf("mode=%s credential=%s: %v, want it to start", c.mode, c.cred, err)
			}
		})
	}
}

// auth.oidc.enabled next to a static JWT is refused whatever auth.mode is, but by config.Load's own rule (the
// legacy jwt must pin its issuer, auth.oidc.issuers[]) which runs before the wiring, so the error does not
// name the extension point. Only the refusal is pinned here.
func TestStartup_RefusesOIDCWithJWT(t *testing.T) {
	for _, mode := range []string{"off", "optional", "required"} {
		t.Run("mode "+mode, func(t *testing.T) {
			if err := boot(t, mode, true, "jwt"); err == nil {
				t.Fatalf("mode=%s oidc on with jwt: started, want a refusal", mode)
			}
		})
	}
}
