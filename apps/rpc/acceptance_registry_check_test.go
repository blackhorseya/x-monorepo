package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/acceptance"
)

func noop(*godog.ScenarioContext, *acceptance.Env) {}

// A BC with no registration, a registration with no BC, a BC without a scenario, or a BC registered twice fails the run, and
// the message names the BC. No BC at all is the template and passes (only the platform's own features run).
func TestCheckRegistered(t *testing.T) {
	reg := func(names ...string) []registration {
		var rs []registration
		for _, n := range names {
			rs = append(rs, registration{bc: n, register: noop})
		}
		return rs
	}
	for _, tc := range []struct {
		name      string
		bcs       []string
		regs      []registration
		scenarios map[string]int
		want      string // "" = ok
	}{
		{"no BC", nil, nil, nil, ""},
		{"all registered", []string{"alpha", "beta"}, reg("beta", "alpha"), map[string]int{"alpha": 2, "beta": 3}, ""},
		{"BC without registration", []string{"alpha", "beta"}, reg("alpha"), map[string]int{"alpha": 2, "beta": 3}, `"beta"`},
		{"registration without BC", []string{"alpha"}, reg("alpha", "ghost"), map[string]int{"alpha": 2}, `"ghost"`},
		{"BC without scenario", []string{"alpha"}, reg("alpha"), map[string]int{"alpha": 0}, `"alpha" has no scenario`},
		{"BC without a features entry", []string{"alpha"}, reg("alpha"), nil, `"alpha" has no scenario`},
		{"registered twice", []string{"alpha"}, reg("alpha", "alpha"), map[string]int{"alpha": 2}, `"alpha" is registered twice`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRegistered(tc.bcs, tc.regs, tc.scenarios)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %s", err, tc.want)
			}
		})
	}
}

// bcsFromWork applies scripts/bcs.jq to `go work edit -json`: pkgs/* modules except kernel and platform.
func TestBCsFromWork(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required: scripts/bcs.jq is the BC rule") // fail closed, never skip
	}
	root := t.TempDir()
	jq, err := os.ReadFile("../../scripts/bcs.jq")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("scripts/bcs.jq", string(jq))
	for _, m := range []string{"pkgs/kernel", "pkgs/platform", "pkgs/alpha", "pkgs/beta", "apps/rpc"} {
		mk(m+"/go.mod", "module example.com/"+m+"\n\ngo 1.27\n")
	}
	mk("go.work", "go 1.27\n\nuse (\n\t./apps/rpc\n\t./pkgs/alpha\n\t./pkgs/beta\n\t./pkgs/kernel\n\t./pkgs/platform\n)\n")

	got, err := bcsFromWork(root)
	if err != nil {
		t.Fatalf("bcsFromWork = %v", err)
	}
	if strings.Join(got, ",") != "alpha,beta" {
		t.Fatalf("bcs = %v, want [alpha beta]", got)
	}

	// jq missing: the run fails and says why, it does not read an empty list as "no BC".
	t.Setenv("PATH", t.TempDir())
	if _, err := bcsFromWork(root); err == nil || !strings.Contains(err.Error(), "jq") {
		t.Fatalf("without jq on PATH: err = %v, want it to mention jq", err)
	}
}
