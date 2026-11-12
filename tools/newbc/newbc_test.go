package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sharedFiles are the files of the real repository the generator reads or edits; a test works on a
// copy of them, never on the repository itself.
var sharedFiles = []string{
	"go.work", "buf.gen.yaml", "scripts/bcs.jq", "pkgs/kernel/go.mod",
	"apps/rpc/go.mod", "apps/rpc/wire.go", "apps/rpc/app.go",
	"apps/migrate/go.mod", "apps/migrate/main.go",
	"apps/worker/go.mod", "apps/worker/wire.go", "apps/worker/consumers.go", "apps/worker/poison.go",
}

func repoCopy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required: scripts/bcs.jq is the BC name rule") // fail closed, never skip
	}
	root := t.TempDir()
	for _, rel := range sharedFiles {
		b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, rel, b)
	}
	for _, dir := range []string{"pkgs/legacy", "protos/legacy"} { // an existing BC, made here: the repository ships none
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func write(t *testing.T, root, rel string, b []byte) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

// snapshot is every path under root (files and directories) with its content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			snap[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(p)
		snap[rel] = string(b)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return snap
}

func gen(root string, args ...string) (code int, stderr string) {
	var out, errb bytes.Buffer
	code = run(append([]string{"--root", root}, args...), &out, &errb)

	return code, errb.String()
}

// The product contract: a refused request changes nothing, not one byte and not one new file.
func TestRefusalChangesNothing(t *testing.T) {
	longName := strings.Repeat("a", maxNameLen+1)
	for _, tc := range []struct {
		name   string
		args   []string
		damage func(t *testing.T, root string)
		want   string // what the error says
	}{
		{"uppercase", []string{"Foo"}, nil, "bcs.jq"},
		{"digit first", []string{"1x"}, nil, "bcs.jq"},
		{"dash", []string{"a-b"}, nil, "bcs.jq"},
		{"go keyword", []string{"type"}, nil, "keyword"},
		{"kernel", []string{"kernel"}, nil, "reserved"},
		{"platform", []string{"platform"}, nil, "reserved"},
		{"main", []string{"main"}, nil, "reserved"},
		{"too long", []string{longName}, nil, "characters"},
		{"existing BC", []string{"legacy"}, nil, "already exists"},
		{"missing anchor", []string{"alpha"}, func(t *testing.T, root string) {
			write(t, root, "buf.gen.yaml", []byte(strings.ReplaceAll(read(t, root, "buf.gen.yaml"), "# new-bc:go-package", "# gone")))
		}, "not found"},
		{"duplicated anchor", []string{"alpha"}, func(t *testing.T, root string) {
			s := read(t, root, "apps/rpc/app.go")
			write(t, root, "apps/rpc/app.go", []byte(strings.Replace(s, "// new-bc:mount-elem", "// new-bc:mount-elem\n\t\t// new-bc:mount-elem", 1)))
		}, "twice"},
		{"unparsable go.mod", []string{"alpha"}, func(t *testing.T, root string) {
			write(t, root, "apps/migrate/go.mod", []byte("this is not a go.mod {{{"))
		}, "parse"},
		{"events: worker anchor missing", []string{"alpha", "--events"}, func(t *testing.T, root string) {
			s := read(t, root, "apps/worker/poison.go")
			write(t, root, "apps/worker/poison.go", []byte(strings.ReplaceAll(s, "// new-bc:schemas", "// gone")))
		}, "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repoCopy(t)
			if tc.damage != nil {
				tc.damage(t, root)
			}
			before := snapshot(t, root)

			code, stderr := gen(root, tc.args...)
			if code != 1 {
				t.Fatalf("exit %d, want 1 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr %q does not say %q", stderr, tc.want)
			}
			after := snapshot(t, root)
			if len(after) != len(before) {
				t.Fatalf("the tree has %d entries after, %d before", len(after), len(before))
			}
			for p, content := range before {
				if got, ok := after[p]; !ok || got != content {
					t.Fatalf("%s changed by a refused request", p)
				}
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	root := repoCopy(t)
	for _, args := range [][]string{nil, {"a", "b"}, {"--nope", "a"}} {
		if code, _ := gen(root, args...); code != 2 {
			t.Fatalf("newbc %v exit %d, want 2", args, code)
		}
	}
}

// One BC without events, then a second one with: each shared file gets one entry per BC, the anchors
// stay (exactly once) for the next one, and the worker is untouched until a BC consumes events.
func TestGenerateTwoBCs(t *testing.T) {
	root := repoCopy(t)
	workerBefore := map[string]string{}
	for _, f := range []string{"apps/worker/go.mod", "apps/worker/wire.go", "apps/worker/consumers.go", "apps/worker/poison.go"} {
		workerBefore[f] = read(t, root, f)
	}

	if code, stderr := gen(root, "alpha"); code != 0 {
		t.Fatalf("alpha: exit %d: %s", code, stderr)
	}
	for f, want := range workerBefore {
		if read(t, root, f) != want {
			t.Fatalf("%s changed by a BC that consumes no events", f)
		}
	}
	for _, f := range []string{"pkgs/alpha/provider.go", "pkgs/alpha/go.mod", "protos/alpha/v1/service.proto", "pkgs/alpha/infra/postgres/migrations/0001_alpha.up.sql"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "pkgs/alpha/adapters/consumer")); err == nil {
		t.Fatal("a BC without --events has a consumer")
	}

	if code, stderr := gen(root, "beta", "--events"); code != 0 {
		t.Fatalf("beta: exit %d: %s", code, stderr)
	}
	for _, f := range []string{"pkgs/beta/adapters/consumer/handler.go", "pkgs/beta/infra/postgres/migrations/0003_inbox.up.sql", "protos/beta/v1/event.proto"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}

	for file, wants := range map[string][]string{
		"go.work":                  {"./pkgs/alpha", "./pkgs/beta"},
		"buf.gen.yaml":             {"path: alpha/v1", "path: beta/v1"},
		"apps/rpc/wire.go":         {"alpha.ProviderSet,", "beta.ProviderSet,", "eventbus.ProvidePublisher,"},
		"apps/rpc/app.go":          {"alpha alpha.RPC,", "beta beta.RPC,", "server.Mount(alpha),", "server.Mount(beta),"},
		"apps/migrate/main.go":     {"alphapg.Migration(),", "betapg.Migration(),"},
		"apps/worker/wire.go":      {"beta.ProviderSet,", "usecasex.NewRecorder,"},
		"apps/worker/consumers.go": {"betaSubs beta.Subscriptions,", "{schema: beta.Schema, subs: betaSubs},"},
		"apps/worker/poison.go":    {"beta.Schema,"},
		"apps/rpc/go.mod":          {"pkgs/alpha => ../../pkgs/alpha", "pkgs/beta => ../../pkgs/beta"},
		"apps/migrate/go.mod":      {"pkgs/alpha => ../../pkgs/alpha", "pkgs/beta => ../../pkgs/beta"},
		"apps/worker/go.mod":       {"pkgs/beta => ../../pkgs/beta"},
	} {
		got := read(t, root, file)
		for _, want := range wants {
			if !strings.Contains(strings.Join(strings.Fields(got), " "), strings.Join(strings.Fields(want), " ")) && !strings.Contains(got, want) {
				t.Errorf("%s lacks %q:\n%s", file, want, got)
			}
		}
	}
	// The template's rpc wires no metrics recorder; the first BC brings it, and the second does not repeat it.
	rpcWire := read(t, root, "apps/rpc/wire.go")
	if n := strings.Count(rpcWire, "usecasex.NewRecorder,"); n != 1 {
		t.Errorf("apps/rpc/wire.go has usecasex.NewRecorder %d times after two BCs, want once:\n%s", n, rpcWire)
	}
	if !strings.Contains(rpcWire, `"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"`) {
		t.Errorf("apps/rpc/wire.go does not import usecasex:\n%s", rpcWire)
	}
	if strings.Contains(read(t, root, "apps/worker/poison.go"), "alpha.Schema") {
		t.Error("worker lists the schema of a BC that consumes no events")
	}
	for file, anchor := range map[string]string{
		"apps/rpc/wire.go": "// new-bc:rpc-providers", "apps/rpc/app.go": "// new-bc:mount-elem", "buf.gen.yaml": "# new-bc:go-package",
		"apps/migrate/main.go": "// new-bc:migration", "apps/worker/wire.go": "// new-bc:worker-providers",
		"apps/worker/consumers.go": "// new-bc:consumer-elem", "apps/worker/poison.go": "// new-bc:schemas",
	} {
		if n := strings.Count(read(t, root, file), anchor); n != 1 {
			t.Errorf("%s has the anchor %q %d times after two BCs, want once", file, anchor, n)
		}
	}
}
