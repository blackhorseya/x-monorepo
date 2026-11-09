package main

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/mod/modfile"
)

//go:embed all:templates
var templates embed.FS

// Options is what `task new-bc` was asked for.
type Options struct {
	Name   string
	Events bool
	Root   string // the repository root
}

// Plan is everything Generate decided: new files and the full new content of edited ones.
// Nothing is on disk until Apply.
type Plan struct {
	New    map[string][]byte // path (slash, relative to the root) → content
	Edit   map[string][]byte
	Edited []string // Edit's paths, in the order they are written
	order  []string // New's paths, in the order they are written
}

// data is what the templates see. Delimiters are <% %> because Go code is full of {{.
type data struct {
	Name      string // the BC: pkgs/<Name>, schema, proto package
	Title     string // the aggregate: Name capitalised
	Module    string // the repository's module path
	GoVersion string
	Events    bool
}

// reserved are pkgs/ directories that are not bounded contexts (the same rule as scripts/bcs.jq and
// pkgs/platform/archtest/imports.go), and a name whose root package could not be imported.
var reserved = map[string]string{
	"kernel":   "pkgs/kernel is shared code, not a bounded context",
	"platform": "pkgs/platform is shared code, not a bounded context",
	"main":     "a package named main cannot be imported by the apps",
}

const maxNameLen = 30 // the name is a Postgres schema and part of every events_<topic> table name

// Generate computes the plan. It reads the repository and writes nothing.
func Generate(o Options) (*Plan, error) {
	if err := checkName(o.Name); err != nil {
		return nil, err
	}
	for _, p := range []string{"pkgs/" + o.Name, "protos/" + o.Name} {
		if _, err := os.Stat(filepath.Join(o.Root, p)); err == nil {
			return nil, fmt.Errorf("%s already exists: bounded context %q is already there", p, o.Name)
		}
	}

	module, err := repoModule(o.Root)
	if err != nil {
		return nil, err
	}
	work, err := readWork(o.Root)
	if err != nil {
		return nil, err
	}
	// The name rule (bcs.jq) runs before any template renders: a bad name must fail with that rule's
	// message, not as invalid Go from a template.
	if err := checkBCS(o, work); err != nil {
		return nil, err
	}
	d := data{Name: o.Name, Title: strings.ToUpper(o.Name[:1]) + o.Name[1:], Module: module, GoVersion: work.Go.Version, Events: o.Events}

	p := &Plan{New: map[string][]byte{}, Edit: map[string][]byte{}}
	if err := p.render(d); err != nil {
		return nil, err
	}
	if err := p.editShared(o, d, work); err != nil {
		return nil, err
	}

	return p, nil
}

// checkName is the part of the name rule that scripts/bcs.jq does not state. The rest (the
// ^[a-z][a-z0-9]*$ allowlist) is bcs.jq's, run on the new go.work in editShared: one rule, one place.
func checkName(name string) error {
	if why, ok := reserved[name]; ok {
		return fmt.Errorf("name %q is reserved: %s", name, why)
	}
	if token.IsKeyword(name) {
		return fmt.Errorf("name %q is a Go keyword and cannot be a package name", name)
	}
	if name == "" || len(name) > maxNameLen {
		return fmt.Errorf("name %q must be 1 to %d characters: it is a Postgres schema name", name, maxNameLen)
	}

	return nil
}

func repoModule(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "pkgs/kernel/go.mod"))
	if err != nil {
		return "", fmt.Errorf("read pkgs/kernel/go.mod (is %s the repository root?): %w", root, err)
	}
	f, err := modfile.ParseLax("pkgs/kernel/go.mod", b, nil)
	if err != nil || f.Module == nil {
		return "", fmt.Errorf("parse pkgs/kernel/go.mod: %v", err)
	}
	mod, ok := strings.CutSuffix(f.Module.Mod.Path, "/pkgs/kernel")
	if !ok {
		return "", fmt.Errorf("pkgs/kernel/go.mod declares %q, want <repo module>/pkgs/kernel", f.Module.Mod.Path)
	}

	return mod, nil
}

func readWork(root string) (*modfile.WorkFile, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		return nil, fmt.Errorf("read go.work: %w", err)
	}
	f, err := modfile.ParseWork("go.work", b, nil)
	if err != nil {
		return nil, fmt.Errorf("parse go.work: %w", err)
	}
	if f.Go == nil {
		return nil, fmt.Errorf("go.work has no go directive")
	}

	return f, nil
}

// render fills templates/common (always) and templates/events (--events). A template path is the
// output path with .tmpl added and __name__ standing for the BC name.
func (p *Plan) render(d data) error {
	dirs := []string{"common"}
	if d.Events {
		dirs = append(dirs, "events")
	}
	for _, dir := range dirs {
		err := fs.WalkDir(templates, "templates/"+dir, func(file string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel := strings.TrimSuffix(strings.TrimPrefix(file, "templates/"+dir+"/"), ".tmpl")
			rel = strings.ReplaceAll(rel, "__name__", d.Name)
			src, err := templates.ReadFile(file)
			if err != nil {
				return err
			}
			t, err := template.New(file).Delims("<%", "%>").Option("missingkey=error").Parse(string(src))
			if err != nil {
				return fmt.Errorf("template %s: %w", file, err)
			}
			var out bytes.Buffer
			if err := t.Execute(&out, d); err != nil {
				return fmt.Errorf("template %s: %w", file, err)
			}
			content := out.Bytes()
			if strings.HasSuffix(rel, ".go") {
				// gofmt also proves the template produced valid Go.
				if content, err = format.Source(content); err != nil {
					return fmt.Errorf("template %s renders invalid Go: %w", file, err)
				}
			}
			if _, dup := p.New[rel]; dup {
				return fmt.Errorf("templates write %s twice", rel)
			}
			p.New[rel] = content
			p.order = append(p.order, rel)

			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Strings(p.order)

	return nil
}

// checkBCS runs bcs.jq over a copy of go.work with the new BC added and requires it to derive the name.
func checkBCS(o Options, work *modfile.WorkFile) error {
	cp, err := modfile.ParseWork("go.work", modfile.Format(work.Syntax), nil)
	if err != nil {
		return fmt.Errorf("go.work: %w", err)
	}
	if err := cp.AddUse("./pkgs/"+o.Name, ""); err != nil {
		return fmt.Errorf("go.work: %w", err)
	}
	names, err := bcsJQ(o.Root, cp)
	if err != nil {
		return err
	}
	if !slices.Contains(names, o.Name) {
		return fmt.Errorf("scripts/bcs.jq derives %v from the new go.work, without %q", names, o.Name)
	}

	return nil
}

// bcsJQ runs scripts/bcs.jq (Taskfile.yml's BCS) over a go.work in `go work edit -json` shape and
// returns the BC names it derives. The name rule lives there and only there.
func bcsJQ(root string, work *modfile.WorkFile) ([]string, error) {
	var js bytes.Buffer
	js.WriteString(`{"Use":[`)
	for i, u := range work.Use {
		if i > 0 {
			js.WriteByte(',')
		}
		fmt.Fprintf(&js, `{"DiskPath":%q}`, u.Path)
	}
	js.WriteString(`]}`)

	cmd := exec.Command("jq", "-r", "-f", filepath.Join(root, "scripts/bcs.jq"))
	cmd.Stdin = &js
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("scripts/bcs.jq refuses the new go.work (the BC name rule): %v: %s", err, strings.TrimSpace(errb.String()))
	}

	return strings.Fields(out.String()), nil
}

// Apply writes the plan. A write that fails midway leaves the files written so far: the error lists them.
func (p *Plan) Apply(root string) error {
	var written []string
	fail := func(err error) error {
		return fmt.Errorf("%w\nwritten before the failure: %s\nrestore with `git restore` (edited files) and `git clean -fd pkgs/<name> protos/<name>` (new ones)", err, strings.Join(written, ", "))
	}
	for _, rel := range p.order {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(abs, p.New[rel], 0o644); err != nil {
			return fail(err)
		}
		written = append(written, rel)
	}
	for _, rel := range p.Edited {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		mode := os.FileMode(0o644)
		if st, err := os.Stat(abs); err == nil {
			mode = st.Mode().Perm()
		}
		if err := os.WriteFile(abs, p.Edit[rel], mode); err != nil {
			return fail(err)
		}
		written = append(written, rel)
	}

	return nil
}
