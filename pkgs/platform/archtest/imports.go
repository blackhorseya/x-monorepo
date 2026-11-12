package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Import rules: the dependency direction between layers and bounded contexts.
// They live here, and not in .golangci.yml, because depguard `pkg` entries are
// prefix matches without wildcards: a rule per BC means every new BC edits the
// rules of every existing one. These rules are spelled once, in terms of "the BC
// this module is" and "any other BC", so adding a BC edits no rule.
//
// Which kind of module a directory is comes from the module path in its go.mod,
// <repo>/pkgs/<name> or <repo>/apps/<name>:
//
//	pkgs/kernel, pkgs/platform   shared: import no BC (test files included)
//	pkgs/<bc>                    a BC: layer rules inside it, no other BC's layers
//	apps/migrate                 composition root that may collect each BC's infra
//	apps/<other>                 composition root
//
// A BC is every pkgs/<name> except kernel and platform; Taskfile.yml's BCS
// applies the same rule to go.work's `use` list. Every module of those kinds has a
// test calling RunImports (a BC reaches it through Run), so each rule runs in the
// module whose files it judges — and in CI whenever that module is affected. A new
// app needs its own arch_test.go: only a module with one is checked.
//
// An import is denied when it equals a denied path or lies below it
// (p == d || p starts with d+"/"). depguard compared plain string prefixes, so it
// also denied a sibling like ".../billingx" for ".../billing"; this does not.
// Generated files are skipped (as golangci-lint skips them); files behind a build
// tag, wire.go's wireinject, are scanned.

// The only two pkgs/ directories that are not bounded contexts.
var sharedDirs = map[string]bool{"kernel": true, "platform": true}

// bcLayers are the directories of a BC another BC (or an app) may not reach into.
// contract is deliberately not one: a BC reads another's contract/.
var bcLayers = []string{"domain", "application", "infra", "adapters"}

type denied struct{ pkg, desc string }

// importRule judges the imports of the files its scope selects.
type importRule struct {
	name  string
	scope func(rel string, test bool) bool
	// deny returns the paths an import must not equal or lie below (nil: nothing to deny).
	deny func(p string) *denied
}

// modKind is what a go.mod's module path says about the module.
type modKind struct {
	repo   string // the repository's module path, e.g. github.com/o/x-monorepo
	parent string // "pkgs" or "apps"
	name   string // directory name under parent
}

func (k modKind) pkgs(rest ...string) string {
	return path.Join(append([]string{k.repo, "pkgs"}, rest...)...)
}

// RunImports applies the import rules of the module at root (the directory with its go.mod).
func RunImports(t *testing.T, root string) {
	t.Helper()
	k, err := moduleKind(root)
	if err != nil {
		t.Fatal(err)
	}
	rules := rulesFor(k)
	if len(rules) == 0 {
		t.Fatalf("no import rules for module %s/%s/%s: only pkgs/* and apps/* modules are checked", k.repo, k.parent, k.name)
	}

	counts := make([]int, len(rules))
	files := 0
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (n == "testdata" || n == "node_modules" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			if p != root {
				if _, serr := os.Stat(filepath.Join(p, "go.mod")); serr == nil {
					return filepath.SkipDir // a nested module is its own module
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly|parser.ParseComments)
		if perr != nil {
			return perr
		}
		if ast.IsGenerated(f) {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		test := strings.HasSuffix(rel, "_test.go")
		files++
		for i, r := range rules {
			if !r.scope(rel, test) {
				continue
			}
			counts[i]++
			for _, imp := range f.Imports {
				ip, uerr := strconv.Unquote(imp.Path.Value)
				if uerr != nil {
					return uerr
				}
				if d := r.deny(ip); d != nil && underPath(ip, d.pkg) {
					t.Errorf("[%s] %s: imports %q (denied %q): %s", r.name, fset.Position(imp.Pos()), ip, d.pkg, d.desc)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// An empty scan is a pass that checked nothing: the module must have files, and each run says what it covered.
	if files == 0 {
		t.Fatalf("scanned no .go file under %s: the rules judged nothing", root)
	}
	for i, r := range rules {
		t.Logf("import rule %-22s module %s/%s: %d files", r.name, k.parent, k.name, counts[i])
	}
}

// underPath reports whether import path p equals d or lies below it.
func underPath(p, d string) bool { return p == d || strings.HasPrefix(p, d+"/") }

// moduleKind reads root/go.mod's module path and splits it into repo, pkgs|apps, name.
func moduleKind(root string) (modKind, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return modKind{}, err
	}

	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		mod := strings.Trim(fields[1], "\"`")
		k := modKind{
			repo:   path.Dir(path.Dir(mod)),
			parent: path.Base(path.Dir(mod)),
			name:   path.Base(mod),
		}
		if k.name == "" || k.name == "." || k.name == "/" {
			break
		}
		return k, nil
	}
	return modKind{}, fmt.Errorf("no usable module line in %s/go.mod", root)
}

// bcOf returns the BC name of an import path under <repo>/pkgs/<name>/..., and the
// directory below it ("" for the BC's root package); ok is false for anything that is
// not inside a BC (kernel, platform, third party).
func (k modKind) bcOf(p string) (name, layer string, ok bool) {
	rest, found := strings.CutPrefix(p, k.pkgs()+"/")
	if !found {
		return "", "", false
	}
	name, below, _ := strings.Cut(rest, "/")
	if name == "" || sharedDirs[name] {
		return "", "", false
	}
	layer, _, _ = strings.Cut(below, "/")
	return name, layer, true
}

func isLayer(l string) bool {
	for _, x := range bcLayers {
		if x == l {
			return true
		}
	}
	return false
}

func rulesFor(k modKind) []importRule {
	switch {
	case k.parent == "pkgs" && sharedDirs[k.name]:
		return []importRule{{
			name:  "shared",
			scope: func(string, bool) bool { return true }, // test files included, as the depguard block did
			deny: func(p string) *denied {
				if _, _, ok := k.bcOf(p); ok {
					return &denied{p, "shared packages must not depend on a bounded context (a BC reaches the platform through a port the platform declares, e.g. server.CredentialResolver, wired in apps)"}
				}
				return nil
			},
		}}
	case k.parent == "pkgs":
		return bcRules(k)
	case k.parent == "apps" && k.name == "migrate":
		return []importRule{{
			name:  "apps-migrate",
			scope: nonTest,
			deny: appDeny(k, map[string]string{
				"domain":      "apps/migrate must not touch domain",
				"application": "apps/migrate must not call use cases",
				"adapters":    "apps/migrate must not touch adapters",
			}),
		}}
	case k.parent == "apps":
		return []importRule{{
			name:  "apps",
			scope: nonTest,
			deny: appDeny(k, map[string]string{
				"domain":      "apps must not touch domain",
				"application": "apps must not call use cases directly",
				"infra":       "apps must not touch infra (except apps/migrate, by explicit exception)",
			}),
		}}
	}
	return nil
}

func nonTest(_ string, test bool) bool { return !test }

// appDeny denies, for every BC, the layers in why (layer → desc).
func appDeny(k modKind, why map[string]string) func(string) *denied {
	return func(p string) *denied {
		if name, l, ok := k.bcOf(p); ok {
			if desc, hit := why[l]; hit {
				return &denied{path.Join(k.pkgs(), name, l), desc}
			}
		}
		return nil
	}
}

// bcRules: the four layers of the BC this module is, and its isolation from every other BC.
func bcRules(k modKind) []importRule {
	own := k.pkgs(k.name)
	under := func(layer string) func(string, bool) bool {
		return func(rel string, test bool) bool { return !test && (rel == layer || strings.HasPrefix(rel, layer+"/")) }
	}
	const (
		pbDesc  = "must not depend on protobuf"
		rpcDesc = "must not depend on transport"
	)
	return []importRule{
		{
			name: "domain", scope: under("domain"),
			deny: denyList(
				denied{own + "/application", "domain must not depend on application"},
				denied{own + "/infra", "domain must not depend on infra"},
				denied{own + "/adapters", "domain must not depend on adapters"},
				denied{own + "/contract", "domain must not depend on the proto contract"},
				denied{k.pkgs("platform"), "domain must not depend on platform"},
				denied{"google.golang.org/protobuf", "domain " + pbDesc},
				denied{"connectrpc.com", "domain " + rpcDesc},
			),
		},
		{
			name: "application", scope: under("application"),
			deny: denyList(
				denied{own + "/infra", "application must not depend on infra"},
				denied{own + "/adapters", "application must not depend on adapters"},
				denied{own + "/contract", "application must not depend on the proto contract (use dto)"},
				denied{k.pkgs("platform", "server"), "application must not depend on transport"},
				denied{"google.golang.org/protobuf", "application " + pbDesc},
				denied{"connectrpc.com", "application " + rpcDesc},
			),
		},
		{
			name: "infra", scope: under("infra"),
			deny: denyList(
				denied{own + "/adapters", "infra must not depend on adapters"},
				denied{own + "/contract", "infra must not depend on the proto contract"},
				denied{own + "/application/usecase", "infra implements ports; it must not call use cases"},
			),
		},
		{
			name: "adapters", scope: under("adapters"),
			deny: denyList(
				denied{own + "/infra", "adapters must not depend on infra (wire them in provider.go)"},
				denied{own + "/application/usecase", "adapters depend on usecasex.UseCase interfaces, not concrete use cases"},
			),
		},
		{
			name:  "bc-isolation",
			scope: nonTest,
			deny: func(p string) *denied {
				if name, l, ok := k.bcOf(p); ok && name != k.name && isLayer(l) {
					return &denied{path.Join(k.pkgs(), name, l), "a bounded context must not import another's " + l + " (read its contract/ through an adapter)"}
				}
				return nil
			},
		},
	}
}

// denyList is a deny func over fixed entries: the first one p equals or lies below.
func denyList(list ...denied) func(string) *denied {
	return func(p string) *denied {
		for i := range list {
			if underPath(p, list[i].pkg) {
				return &list[i]
			}
		}
		return nil
	}
}
