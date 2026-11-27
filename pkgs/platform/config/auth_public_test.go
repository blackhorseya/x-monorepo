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
	for name, file := range map[string]string{
		"list":          "auth:\n  public: [/a.v1.S/Get]\n",
		"empty list":    "auth:\n  public: []\n",
		"null":          "auth:\n  public:\n",
		"empty string":  "auth:\n  public: \"\"\n",
		"empty mapping": "auth:\n  public: {}\n",
		"mapping":       "auth:\n  public: {a: b}\n",
		"other case":    "Auth:\n  Public: []\n",
		"dotted key":    "auth.public: []\n",
	} {
		t.Run("config file "+name, func(t *testing.T) {
			chdirTemp(t)
			if err := os.WriteFile("config.yaml", []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
			refused(t)
		})
	}
	for name, value := range map[string]string{"set": "/a.v1.S/Get", "empty": ""} {
		t.Run("env "+name, func(t *testing.T) {
			chdirTemp(t)
			t.Setenv("APP_AUTH_PUBLIC", value)
			refused(t)
		})
	}
	// Positive control: an auth block without the key loads.
	t.Run("absent", func(t *testing.T) {
		chdirTemp(t)
		if err := os.WriteFile("config.yaml", []byte("auth:\n  mode: \"off\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(); err != nil {
			t.Fatal(err)
		}
	})
}
