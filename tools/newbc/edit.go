package main

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/ast/astutil"
	"gopkg.in/yaml.v3"
)

// anchor is a comment line a shared file keeps for this generator: `// new-bc:<id>` (`# new-bc:<id>` in
// YAML). New entries are inserted just above it, so the next BC lands below the previous one. A line
// matches when, trimmed, it is exactly the comment; the anchor must occur exactly once.
const anchorPrefix = "new-bc:"

// insertAt puts lines above the single line that is exactly `<comment> new-bc:<id>`, indented like it.
func insertAt(file string, src []byte, comment, id string, lines ...string) ([]byte, error) {
	want := comment + " " + anchorPrefix + id
	text := strings.Split(string(src), "\n")
	at := -1
	for i, l := range text {
		if strings.TrimSpace(l) == want {
			if at >= 0 {
				return nil, fmt.Errorf("%s: anchor %q occurs twice (lines %d and %d)", file, want, at+1, i+1)
			}
			at = i
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("%s: anchor %q not found", file, want)
	}
	indent := text[at][:len(text[at])-len(strings.TrimLeft(text[at], " \t"))]
	ins := make([]string, len(lines))
	for i, l := range lines {
		ins[i] = indent + l
	}

	return []byte(strings.Join(slices.Concat(text[:at], ins, text[at:]), "\n")), nil
}

// funcContains reports whether the function starting at `func <name>(` mentions s before its closing "\n}".
func funcContains(src []byte, name, s string) bool {
	text := string(src)
	i := strings.Index(text, "func "+name+"(")
	if i < 0 {
		return false
	}
	body := text[i:]
	if j := strings.Index(body, "\n}\n"); j >= 0 {
		body = body[:j]
	}

	return strings.Contains(body, s)
}

// edit is one change to one shared file: new content from the old.
type edit func(src []byte) ([]byte, error)

func (p *Plan) editFile(root, rel string, fn edit) error {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("read %s: %w", rel, err)
	}
	out, err := fn(src)
	if err != nil {
		return err
	}
	p.Edit[rel] = out
	p.Edited = append(p.Edited, rel)

	return nil
}

// goEdit inserts at anchors, then adds the imports, and parses the result: an edit that does not
// leave valid Go is refused before anything is written.
func goEdit(rel string, inserts func([]byte) ([]byte, error), imports map[string]string) edit {
	return func(src []byte) ([]byte, error) {
		out, err := inserts(src)
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, out, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s does not parse after the insertion: %w", rel, err)
		}
		for _, imp := range sortedKeys(imports) { // imports: path → name ("" = the package's own)
			if name := imports[imp]; name == "" {
				astutil.AddImport(fset, f, imp)
			} else {
				astutil.AddNamedImport(fset, f, name, imp)
			}
		}
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, f); err != nil {
			return nil, fmt.Errorf("format %s: %w", rel, err)
		}

		return buf.Bytes(), nil
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)

	return ks
}

// editShared adds the new BC to every shared file.
func (p *Plan) editShared(o Options, d data, work *modfile.WorkFile) error {
	n, mod := d.Name, d.Module
	bcPkg := mod + "/pkgs/" + n

	// go.work: one `use`, and scripts/bcs.jq (the BC name rule) must accept the result and derive the name.
	if err := work.AddUse("./pkgs/"+n, ""); err != nil {
		return fmt.Errorf("go.work: %w", err)
	}
	work.SortBlocks()
	work.Cleanup()
	names, err := bcsJQ(o.Root, work)
	if err != nil {
		return err
	}
	if !slices.Contains(names, n) {
		return fmt.Errorf("scripts/bcs.jq derives %v from the new go.work, without %q", names, n)
	}
	p.Edit["go.work"] = modfile.Format(work.Syntax)
	p.Edited = append(p.Edited, "go.work")

	// apps/*/go.mod: require + replace. worker only needs a BC that consumes events.
	apps := []string{"rpc", "migrate"}
	if o.Events {
		apps = append(apps, "worker")
	}
	for _, app := range apps {
		rel := "apps/" + app + "/go.mod"
		err := p.editFile(o.Root, rel, func(src []byte) ([]byte, error) {
			f, err := modfile.Parse(rel, src, nil)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", rel, err)
			}
			if err := f.AddRequire(bcPkg, "v0.0.0"); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			if err := f.AddReplace(bcPkg, "", "../../pkgs/"+n, ""); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			f.Cleanup()

			return modfile.Format(f.Syntax), nil
		})
		if err != nil {
			return err
		}
	}

	// buf.gen.yaml: the go_package override of protos/<n>/v1.
	err = p.editFile(o.Root, "buf.gen.yaml", func(src []byte) ([]byte, error) {
		out, err := insertAt("buf.gen.yaml", src, "#", "go-package",
			"- file_option: go_package",
			"  path: "+n+"/v1",
			"  value: "+bcPkg+"/contract/v1;"+n+"v1",
		)
		if err != nil {
			return nil, err
		}

		return out, checkBufGen(out, n+"/v1")
	})
	if err != nil {
		return err
	}

	// apps/rpc: the BC's providers and its mount (NewMounts: one parameter, one element).
	rpcWire := func(src []byte) ([]byte, error) {
		out, err := insertAt("apps/rpc/wire.go", src, "//", "rpc-providers", n+".ProviderSet,")
		if err != nil {
			return nil, err
		}
		if !funcContains(out, "Initialize", "usecasex.NewRecorder") {
			// the use cases' metrics recorder every BC's ProviderSet needs, once for all BCs (the template's
			// rpc wires no BC, so it carries none until the first one is added)
			out, err = insertAt("apps/rpc/wire.go", out, "//", "rpc-providers", "usecasex.NewRecorder,")
			if err != nil {
				return nil, err
			}
		}
		if o.Events && !funcContains(out, "Initialize", "eventbus.ProvidePublisher") {
			// the outbox publisher the BC's use cases publish through, once for all event BCs
			return insertAt("apps/rpc/wire.go", out, "//", "rpc-providers", "eventbus.ProvidePublisher, // outbox of the event BCs")
		}

		return out, nil
	}
	rpcWireImports := map[string]string{bcPkg: "", mod + "/pkgs/platform/usecasex": ""}
	if o.Events {
		rpcWireImports[mod+"/pkgs/platform/eventbus"] = ""
	}
	if err := p.editFile(o.Root, "apps/rpc/wire.go", goEdit("apps/rpc/wire.go", rpcWire, rpcWireImports)); err != nil {
		return err
	}
	err = p.editFile(o.Root, "apps/rpc/app.go", goEdit("apps/rpc/app.go", func(src []byte) ([]byte, error) {
		out, err := insertAt("apps/rpc/app.go", src, "//", "mount-param", n+" "+n+".RPC,")
		if err != nil {
			return nil, err
		}

		return insertAt("apps/rpc/app.go", out, "//", "mount-elem", "server.Mount("+n+"),")
	}, map[string]string{bcPkg: ""}))
	if err != nil {
		return err
	}

	// apps/migrate: the BC's migrations.
	err = p.editFile(o.Root, "apps/migrate/main.go", goEdit("apps/migrate/main.go", func(src []byte) ([]byte, error) {
		return insertAt("apps/migrate/main.go", src, "//", "migration", n+"pg.Migration(),")
	}, map[string]string{bcPkg + "/infra/postgres": n + "pg"}))
	if err != nil {
		return err
	}

	if !o.Events {
		return nil
	}

	// apps/worker (event BCs only): providers, the consumer and its schema.
	workerWire := func(src []byte) ([]byte, error) {
		out, err := insertAt("apps/worker/wire.go", src, "//", "worker-providers", n+".ProviderSet,")
		if err != nil {
			return nil, err
		}
		if !funcContains(out, "Initialize", "usecasex.NewRecorder") {
			// the use cases' metrics recorder, once for all event BCs
			return insertAt("apps/worker/wire.go", out, "//", "worker-providers", "usecasex.NewRecorder,")
		}

		return out, nil
	}
	if err := p.editFile(o.Root, "apps/worker/wire.go", goEdit("apps/worker/wire.go", workerWire, map[string]string{bcPkg: "", mod + "/pkgs/platform/usecasex": ""})); err != nil {
		return err
	}
	err = p.editFile(o.Root, "apps/worker/consumers.go", goEdit("apps/worker/consumers.go", func(src []byte) ([]byte, error) {
		out, err := insertAt("apps/worker/consumers.go", src, "//", "consumer-param", n+"Subs "+n+".Subscriptions,")
		if err != nil {
			return nil, err
		}

		return insertAt("apps/worker/consumers.go", out, "//", "consumer-elem", "{schema: "+n+".Schema, subs: "+n+"Subs},")
	}, map[string]string{bcPkg: ""}))
	if err != nil {
		return err
	}

	return p.editFile(o.Root, "apps/worker/poison.go", goEdit("apps/worker/poison.go", func(src []byte) ([]byte, error) {
		return insertAt("apps/worker/poison.go", src, "//", "schemas", n+".Schema,")
	}, map[string]string{bcPkg: ""}))
}

// checkBufGen parses buf.gen.yaml and requires exactly one go_package override for path.
func checkBufGen(src []byte, path string) error {
	var doc struct {
		Managed struct {
			Override []struct {
				FileOption string `yaml:"file_option"`
				Path       string `yaml:"path"`
				Value      string `yaml:"value"`
			} `yaml:"override"`
		} `yaml:"managed"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return fmt.Errorf("buf.gen.yaml does not parse after the insertion: %w", err)
	}
	n := 0
	for _, o := range doc.Managed.Override {
		if o.FileOption == "go_package" && o.Path == path {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("buf.gen.yaml has %d go_package overrides for %s after the insertion, want 1", n, path)
	}

	return nil
}
