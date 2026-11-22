package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gherkin "github.com/cucumber/gherkin/go/v42"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/acceptance"
)

// registration is one bounded context's acceptance: its name and the steps that register its features. Its
// features are pkgs/<bc>/acceptance/features/*.feature.
type registration struct {
	bc       string
	register acceptance.Register
}

// bcsFromWork is the bounded contexts go.work lists, by scripts/bcs.jq (the one rule for what a BC is, shared with
// Taskfile.yml's gen-openapi and tools/newbc). It needs go and jq: a missing one fails the run, it is never read as "no BC".
func bcsFromWork(root string) ([]string, error) {
	for _, tool := range []string{"jq", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s is required to list the bounded contexts (scripts/bcs.jq is the rule) and is not on PATH", tool)
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	work := exec.Command("go", "work", "edit", "-json")
	work.Dir = abs
	work.Env = append(os.Environ(), "GOWORK="+filepath.Join(abs, "go.work")) // go.work of root, never one found above it
	var js, workErr bytes.Buffer
	work.Stdout, work.Stderr = &js, &workErr
	if err := work.Run(); err != nil {
		return nil, fmt.Errorf("go work edit -json in %s: %w: %s", root, err, strings.TrimSpace(workErr.String()))
	}

	q := exec.Command("jq", "-r", "-f", filepath.Join(abs, "scripts/bcs.jq"))
	q.Stdin = &js
	var out, qErr bytes.Buffer
	q.Stdout, q.Stderr = &out, &qErr
	if err := q.Run(); err != nil {
		return nil, fmt.Errorf("scripts/bcs.jq refuses go.work: %w: %s", err, strings.TrimSpace(qErr.String()))
	}

	return strings.Fields(out.String()), nil
}

// checkRegistered requires the BCs go.work lists and the registrations to be the same set, each BC registered once,
// and every BC to hold at least one scenario (scenarios: BC → how many its features hold). A BC added without its
// acceptance, or an acceptance for a BC that is gone, fails the run instead of being skipped. With no BC, nothing is checked.
func checkRegistered(bcs []string, regs []registration, scenarios map[string]int) error {
	var errs []error
	seen := map[string]int{}
	for _, r := range regs {
		seen[r.bc]++
	}
	for _, bc := range bcs {
		switch {
		case seen[bc] == 0:
			errs = append(errs, fmt.Errorf("bounded context %q has no acceptance registered: add it to registrations in apps/rpc/acceptance_test.go (task new-bc does, at the new-bc:acceptance anchor)", bc))
		case seen[bc] > 1:
			errs = append(errs, fmt.Errorf("bounded context %q is registered twice", bc))
		}
		if scenarios[bc] == 0 {
			errs = append(errs, fmt.Errorf("bounded context %q has no scenario under pkgs/%s/acceptance/features", bc, bc))
		}
	}
	for _, r := range regs {
		if !slices.Contains(bcs, r.bc) {
			errs = append(errs, fmt.Errorf("acceptance is registered for %q, which is not a bounded context in go.work", r.bc))
		}
	}

	return errors.Join(errs...)
}

// featureStats is what the .feature files of one directory hold.
type featureStats struct {
	scenarios int // pickles: an outline counts once per example row
	worker    int // of them, tagged @worker (they get a worker subprocess)
}

// scanFeatures counts the scenarios of the .feature files directly under dir (none, if dir does not exist).
func scanFeatures(t *testing.T, dir string) featureStats {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.feature"))
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	newID := func() string { next++; return fmt.Sprint(next) }
	var st featureStats
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := gherkin.ParseGherkinDocument(bytes.NewReader(raw), newID)
		if err != nil {
			t.Fatalf("acceptance: parse %s: %v", f, err)
		}
		for _, pk := range gherkin.Pickles(*doc, f, newID) {
			st.scenarios++
			for _, tag := range pk.Tags {
				if tag.Name == workerTag {
					st.worker++
					break
				}
			}
		}
	}

	return st
}

// workerTag marks a scenario that needs an apps/worker subprocess against its database.
const workerTag = "@worker"
