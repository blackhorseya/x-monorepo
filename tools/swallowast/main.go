// Command swallowast is the `default` and `fnctx` half of scripts/check-exit-swallow.sh (#472): it judges where a command
// substitution may stand in a shell script, on a real syntax tree (mvdan.cc/sh/v3/syntax) instead of a line scanner.
//
//	swallowast <file>...      prints "file:line: rule: code   [why]" per finding
//
// Exit 0 = no finding, 1 = findings, 2 = the tool itself failed (a file that does not parse is never reported as clean).
//
// The rules are position whitelists, not lists of bad shapes (see check.go and docs/ci.md): a command substitution (or backquote) is
// accepted ONLY as the complete right-hand side of a plain assignment, or as the label idiom `$([ test ] && echo a || echo b)`; whatever
// stands inside it is itself a closed whitelist. Everywhere else it is a finding, because the status of the command is replaced by the
// status of whatever it is an argument of.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// writer remembers the first failed write: a checker that cannot print its findings (a closed pipe, a full disk) must not exit 0 or 1 as if it had.
type writer struct {
	w   io.Writer
	err error
}

func (w *writer) printf(format string, args ...any) {
	if _, err := fmt.Fprintf(w.w, format, args...); err != nil && w.err == nil {
		w.err = err
	}
}

func run(files []string, stdout, stderr io.Writer) int {
	out, errw := &writer{w: stdout}, &writer{w: stderr}
	code := check(files, out, errw)
	if out.err != nil || errw.err != nil {
		return 2
	}
	return code
}

func check(files []string, out, errw *writer) int {
	if len(files) == 0 {
		errw.printf("swallowast: no file given\n")
		return 2
	}
	found := false
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			errw.printf("swallowast: %v\n", err)
			return 2
		}
		fs, err := checkSource(name, src)
		if err != nil {
			errw.printf("swallowast: %v\n", err)
			return 2
		}
		lines := strings.Split(string(src), "\n")
		for _, f := range fs {
			found = true
			code := ""
			if f.line >= 1 && int(f.line) <= len(lines) {
				code = strings.TrimLeft(lines[f.line-1], " \t")
			}
			out.printf("%s:%d: %s: %s   [%s]\n", name, f.line, f.rule, code, f.why)
		}
	}
	if found {
		return 1
	}
	return 0
}

// checkSource parses one file as bash and returns its findings ordered by line.
func checkSource(name string, src []byte) ([]finding, error) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(bytes.NewReader(src), name)
	if err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", name, err)
	}
	c := &checker{}
	c.walk(f)
	sort.SliceStable(c.out, func(i, j int) bool { return c.out[i].line < c.out[j].line })
	return c.out, nil
}
