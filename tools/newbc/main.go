// Command newbc generates a bounded context: `task new-bc -- <name> [--events]`.
//
// It writes pkgs/<name> and protos/<name> from templates/, and adds one entry for the new BC to
// every shared file (go.work, buf.gen.yaml, apps/*/go.mod, the registration lists of apps/rpc,
// apps/worker and apps/migrate). It computes every file in memory first and writes only when all
// of that succeeded: a refused name, an existing BC, a missing or duplicated anchor or an
// unparsable file changes nothing. It stops there: `task new-bc` then runs tidy, gen and wire.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = "usage: newbc [--events] [--root <repo>] <name>"

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "newbc: %v\n%s\n", err, usage)
		return 2
	}

	plan, err := Generate(opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "newbc: %v\n(nothing was changed)\n", err)
		return 1
	}
	if err := plan.Apply(opts.Root); err != nil {
		_, _ = fmt.Fprintf(stderr, "newbc: %v\n", err)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "newbc: created pkgs/%[1]s and protos/%[1]s, edited %d shared files\n", opts.Name, len(plan.Edited))
	for _, p := range plan.Edited {
		_, _ = fmt.Fprintf(stdout, "  edited %s\n", p)
	}

	return 0
}

// parseArgs takes flags and the one name in any order (`task new-bc -- ticket --events`).
func parseArgs(args []string) (Options, error) {
	opts := Options{Root: "."}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--events":
			opts.Events = true
		case a == "--root" && i+1 < len(args):
			i++
			opts.Root = args[i]
		case strings.HasPrefix(a, "--root="):
			opts.Root = strings.TrimPrefix(a, "--root=")
		case strings.HasPrefix(a, "-"):
			return opts, fmt.Errorf("unknown flag %q", a)
		case opts.Name != "":
			return opts, fmt.Errorf("one name only, got %q and %q", opts.Name, a)
		default:
			opts.Name = a
		}
	}
	if opts.Name == "" {
		return opts, errors.New("a name is required")
	}

	return opts, nil
}
