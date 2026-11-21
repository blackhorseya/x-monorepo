package archtest_test

import (
	"go/parser"
	"go/token"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/platform/archtest"
)

// domainRule returns the DefaultRules entry with the given name.
func domainRule(t *testing.T, name string) archtest.Rule {
	t.Helper()
	for _, r := range archtest.DefaultRules() {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("rule %q is not in DefaultRules", name)
	return archtest.Rule{}
}

// TestDomainNoSetters: a Set followed by an upper-case letter is a setter; a name that merely
// starts with the letters "Set" (Settlement…, Setup, Settings) is not.
func TestDomainNoSetters(t *testing.T) {
	rule := domainRule(t, "domain-no-setters")

	cases := []struct {
		name   string
		method string
		want   bool // reported as a setter
	}{
		{"setter", "SetName", true},
		{"setter with a short field", "SetX", true},
		{"getter of a BC named settlement", "SettlementID", false},
		{"word that starts with Set", "Setup", false},
		{"plural noun", "Settings", false},
		// Known boundary: only the name is read, and "_" is not an upper-case letter, so Set_x passes.
		{"underscore after Set", "Set_x", false},
		{"unexported setter", "setName", false},
		{"bare Set", "Set", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package p\n\ntype T struct{ x int }\n\nfunc (x *T) " + tc.method + "() {}\n"
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "fixture.go", src, 0)
			if err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			var got []string
			rule.Check(fset, f, func(_ token.Pos, msg string) { got = append(got, msg) })
			if (len(got) > 0) != tc.want {
				t.Errorf("%s reported = %v (%v), want %v", tc.method, len(got) > 0, got, tc.want)
			}
		})
	}
}
