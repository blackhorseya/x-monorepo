package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/config"
)

// auth.public is gone: what is public is declared on the method in the proto. viper drops a key nothing reads
// without a word, so a config that still sets it must be refused, from either of the two ways in.
func TestLoad_RefusesAuthPublic(t *testing.T) {
	refused := func(t *testing.T) {
		t.Helper()
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "auth.public") {
			t.Fatalf("config.Load() = %v, want a refusal naming auth.public", err)
		}
	}
	t.Run("config file", func(t *testing.T) {
		chdirTemp(t)
		if err := os.WriteFile("config.yaml", []byte("auth:\n  public: [/a.v1.S/Get]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		refused(t)
	})
	t.Run("env", func(t *testing.T) {
		chdirTemp(t)
		t.Setenv("APP_AUTH_PUBLIC", "/a.v1.S/Get")
		refused(t)
	})
}
