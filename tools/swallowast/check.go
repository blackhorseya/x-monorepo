package main

import (
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// finding is one violation. rule is "default" (a command substitution in a position, or with a content, that is not whitelisted, or a
// handler that is not whitelisted) or "fnctx" (inside a function body the assignment must carry a whitelisted handler).
type finding struct {
	line uint
	rule string
	why  string
}

type checker struct {
	stack []syntax.Node // the ancestors of the node being visited, outermost first
	out   []finding
	eo    errexitInfo
}

func (c *checker) report(n syntax.Node, rule, why string) {
	if rule == "fnctx" {
		why = "in " + c.functionName() + "(): " + why
	}
	c.out = append(c.out, finding{line: n.Pos().Line(), rule: rule, why: why})
}

// functionName is the name of the innermost function whose body holds the node being judged.
func (c *checker) functionName() string {
	for i := len(c.stack) - 1; i >= 0; i-- {
		if fd, ok := c.stack[i].(*syntax.FuncDecl); ok && fd.Name != nil {
			return fd.Name.Value
		}
	}
	return "?"
}

func (c *checker) walk(f *syntax.File) {
	c.eo = scanErrexit(f)
	syntax.Walk(f, func(n syntax.Node) bool {
		if n == nil {
			c.stack = c.stack[:len(c.stack)-1]
			return true
		}
		if cs, ok := n.(*syntax.CmdSubst); ok {
			c.judgeSubst(cs)
		}
		c.stack = append(c.stack, n)
		return true
	})
}

// judgeSubst is the whole position rule, one reason per substitution (the first that fails):
//
//  1. a substitution inside another substitution is a finding (the inner status is the argument of the outer command)
//  2. the one exempted node (`. "$(dirname "$0")/../../../scripts/best-effort.sh"`, matched field by field)
//  3. the label idiom is a whitelisted CONTENT; every other substitution's content must be one `&&` chain of simple commands
//  4. the position: the complete right-hand side of a plain assignment (x=$(…) or x="$(…)") whose list context and handler pass
//     listContext; or the label idiom anywhere. Every other position is a finding.
func (c *checker) judgeSubst(cs *syntax.CmdSubst) {
	if c.insideSubst() {
		c.report(cs, "default", "a substitution nested inside another substitution: the inner status becomes an argument of the outer command")
		return
	}
	if c.isSourceException() {
		return
	}
	label := isLabel(cs)
	if !label {
		if why := contentReason(cs); why != "" {
			c.report(cs, "default", why)
			return
		}
	}
	si, ok := c.assignStmt(cs)
	if !ok {
		if label {
			return
		}
		c.report(cs, "default", "a command substitution is accepted only as the complete right-hand side of an assignment (x=$(…)) or as the label idiom: here its status is lost")
		return
	}
	rule, why := c.listContext(si, label)
	if why != "" {
		c.report(cs, rule, why)
	}
}

// insideSubst: any ancestor is a command substitution.
func (c *checker) insideSubst() bool {
	for _, n := range c.stack {
		if _, ok := n.(*syntax.CmdSubst); ok {
			return true
		}
	}
	return false
}

func (c *checker) inFunction() bool {
	for _, n := range c.stack {
		if _, ok := n.(*syntax.FuncDecl); ok {
			return true
		}
	}
	return false
}

// assignStmt: cs is the complete value of a plain assignment statement (`x=$(…)`, `x="$(…)"`; no `+=`, no index, no array, no prefix
// assignment on a command, no second assignment). Returns the index in the stack of that assignment's *Stmt.
func (c *checker) assignStmt(cs *syntax.CmdSubst) (int, bool) {
	i := len(c.stack) - 1
	var w *syntax.Word
	if i >= 0 {
		w, _ = c.stack[i].(*syntax.Word)
	}
	if w == nil {
		if dq, ok := c.at(i).(*syntax.DblQuoted); ok && len(dq.Parts) == 1 && !dq.Dollar {
			i--
			w, _ = c.at(i).(*syntax.Word)
		}
	}
	if w == nil || len(w.Parts) != 1 {
		return 0, false
	}
	a, ok := c.at(i - 1).(*syntax.Assign)
	if !ok || a.Value != w || a.Append || a.Naked || a.Index != nil || a.Array != nil || a.Name == nil {
		return 0, false
	}
	ce, ok := c.at(i - 2).(*syntax.CallExpr)
	if !ok || len(ce.Args) != 0 || len(ce.Assigns) != 1 {
		return 0, false
	}
	if _, ok := c.at(i - 3).(*syntax.Stmt); !ok {
		return 0, false
	}
	return i - 3, true
}

func (c *checker) at(i int) syntax.Node {
	if i < 0 || i >= len(c.stack) {
		return nil
	}
	return c.stack[i]
}

// listContext judges the AND-OR list the assignment stands in. The status of the assignment is that of its substitution, so it must be
// the status of the list (the last element of an `&&` chain, or followed by a whitelisted `|| handler` that ends the list) and the
// list must run in this shell: not in the background, not a pipeline operand, not a subshell.
func (c *checker) listContext(si int, label bool) (rule, why string) {
	rule = "default"
	inFn := c.inFunction()
	if inFn {
		rule = "fnctx"
	}
	var handler *syntax.Stmt
	cur := si
	negated := false
	for {
		st := c.at(cur).(*syntax.Stmt)
		if st.Background || st.Coprocess {
			return "default", "the assignment is in the background or a coprocess: its status is not the status of the list"
		}
		negated = negated || st.Negated
		b, ok := c.at(cur - 1).(*syntax.BinaryCmd)
		if !ok {
			break
		}
		if _, ok := c.at(cur - 2).(*syntax.Stmt); !ok {
			return "default", "the assignment is an operand of an unexpected list"
		}
		if handler != nil {
			return "default", "the handler does not end the list: `&&`, `||` or a pipe follows it, so `exit` / `return` is not what the rest of the list sees"
		}
		switch {
		case b.Op == syntax.AndStmt && b.Y == st: // `a && x=$(cmd)`: the last of the chain, its status is the chain's
		case b.Op == syntax.OrStmt && b.Y == st: // `a || x=$(cmd)`: likewise
		case b.Op == syntax.OrStmt && b.X == st:
			handler = b.Y // a second one cannot come: climbing past a handler is refused above
		case b.Op == syntax.AndStmt && b.X == st: // `x=$(cmd) && use`: no handler → fnctx below in a function; at top level the value is only used when cmd succeeded (#466 r5)
		default:
			return "default", "the assignment is an operand of a pipeline: it runs in a subshell and its status is lost"
		}
		cur -= 2
	}
	if handler == nil && !negated {
		if ok, why := c.conditionReason(cur, assignName(c.at(si).(*syntax.Stmt))); ok {
			return "", ""
		} else if why != "" {
			return rule, why
		}
	}
	if negated {
		return rule, "the assignment is negated (`! x=$(cmd)`): the failure branch is the then-branch and is not judged"
	}
	if handler == nil {
		if inFn {
			return "fnctx", "a function body must check this substitution's status itself — errexit may be off wherever the function is called"
		}
		if why := c.errexitReason(si); why != "" && !label { // the label idiom cannot fail: its value is a or b whatever the test says
			return "default", why
		}
		return "", ""
	}
	if why := c.detached(cur); why != "" {
		return "default", why
	}
	if why := handlerReason(handler, false, !inFn); why != "" {
		if inFn {
			c.report(c.at(si), "default", why) // the handler whitelist is the same everywhere; in a function the missing / wrong check is a fnctx finding as well
		}
		return rule, why
	}
	return "", ""
}

// conditionReason judges an assignment that is (part of) the condition of a compound command. ok is true when it is whitelisted:
//
//	if / elif   the whole chain must have an `else` whose last statement is `exit N` / `return N` (literal 1..255; bare or `$?` only when the
//	            else block is that one statement): a failed assignment goes to the else, which stops. Without an else, or with an else that
//	            does not stop, the code after the `fi` uses the value of the failed run.
//	until       allowed: the loop is only left when the condition succeeded, so the value is the successful run's.
//	while       never: the loop is left by the failure, and the value after it is the failed run's.
//
// When the node is not a condition at all, ok is false and why is empty.
func (c *checker) conditionReason(cur int, name string) (ok bool, why string) {
	st := c.at(cur).(*syntax.Stmt)
	switch p := c.at(cur - 1).(type) {
	case *syntax.IfClause:
		if !isLastStmt(p.Cond, st) {
			return false, ""
		}
		if why := elseStops(p, name); why != "" {
			return false, why
		}
		return true, ""
	case *syntax.WhileClause:
		if !isLastStmt(p.Cond, st) {
			return false, ""
		}
		if p.Until {
			return true, ""
		}
		return false, "the assignment is the condition of a `while`: the loop is left by the failure and the code after it uses the failed run's value"
	}
	return false, ""
}

// assignName: the variable a plain assignment statement sets ("" when it is not one).
func assignName(s *syntax.Stmt) string {
	if ce, ok := s.Cmd.(*syntax.CallExpr); ok && len(ce.Assigns) == 1 && ce.Assigns[0].Name != nil {
		return ce.Assigns[0].Name.Value
	}
	return ""
}

// elseStops: what runs after the assignment's own clause FAILED. Exactly one of
//
//	(a) the final `else`, ending in `exit N` / `return N` (a single statement may be bare or `$?`; with more, every earlier one is an echo / printf);
//	(b) an `elif` whose condition is ONE plain assignment `name=$(…)` to the SAME variable (try A, else B), and the same rule again for that clause.
//
// Everything else is red: any other `elif` runs ITS then-branch with the half-printed value of the failed run (`elif true`, `elif [ -n "$v" ]`, `elif w=$(…)`).
func elseStops(ic *syntax.IfClause, name string) string {
	next := ic.Else
	if next == nil {
		return "the if / elif chain has no `else`: after the `fi` the value of a failed assignment is used"
	}
	if len(next.Cond) != 0 { // an elif
		if !isRetryCondition(next.Cond, name) {
			return "the clause that runs after the failed assignment must be the final `else` or an `elif` whose whole condition is an assignment to the same variable: any other `elif` runs its then-branch with the failed run's value"
		}
		return "" // that elif holds its own assignment, which is judged on its own (same rule for what runs after IT failed)
	}
	if len(next.Then) == 1 {
		return stopStmt(next.Then[0], false, true)
	}
	if why := braceReason(&syntax.Block{Stmts: next.Then}, false); why != "" {
		return "the else block of the if / elif chain must end in `exit N` / `return N` (literal 1..255): " + why
	}
	return ""
}

// isRetryCondition: the condition is exactly one plain assignment `name=$(…)` to the variable the first clause set.
func isRetryCondition(list []*syntax.Stmt, name string) bool {
	if len(list) != 1 || name == "" {
		return false
	}
	s := list[0]
	ce, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 || len(ce.Args) != 0 || len(ce.Assigns) != 1 {
		return false
	}
	a := ce.Assigns[0]
	return a.Name != nil && a.Name.Value == name && !a.Append && a.Index == nil && a.Array == nil && a.Value != nil && hasSubst(a.Value)
}

// stopStmt: one statement that is `exit` / `return`, with nothing, `$?` (allowed when bare is true) or a literal 1..255.
func stopStmt(s *syntax.Stmt, exitOnly, bare bool) string {
	if s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 {
		return "the else block's statement is negated, in the background or has a redirection"
	}
	ce, ok := s.Cmd.(*syntax.CallExpr)
	if !ok {
		return "the else block of the if / elif chain must end in `exit N` / `return N`"
	}
	if !bare {
		if _, ok := exitVerb(ce, exitOnly); !ok || len(ce.Args) != 2 || !exitLiteral(ce.Args[1]) {
			return "the else block must end in `exit N` / `return N` with a literal N of 1..255"
		}
		return ""
	}
	return plainExit(ce, exitOnly)
}

func containsStmt(list []*syntax.Stmt, st *syntax.Stmt) bool {
	for _, x := range list {
		if x == st {
			return true
		}
	}
	return false
}

// isLastStmt: st is the last statement of a list (the status of a condition list is the status of its last statement).
func isLastStmt(list []*syntax.Stmt, st *syntax.Stmt) bool {
	return len(list) > 0 && list[len(list)-1] == st
}

// detached: the list that holds the handler runs somewhere `exit` / `return` only leaves a subshell (a background job, a pipeline
// operand, a subshell, a coprocess, a nested substitution), so the handler does not stop the script.
func (c *checker) detached(top int) string {
	for k := top - 1; k >= 0; k-- {
		switch n := c.stack[k].(type) {
		case *syntax.FuncDecl:
			return ""
		case *syntax.CoprocClause, *syntax.ProcSubst, *syntax.CmdSubst:
			return "the handler's list runs in a coprocess or a process / command substitution, so `exit` / `return` only leaves that subshell"
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				return "the handler's list is a pipeline operand: `exit` / `return` only leaves that subshell"
			}
		case *syntax.Stmt:
			if n.Background || n.Coprocess {
				return "the handler's list runs in the background: `exit` / `return` only leaves that subshell"
			}
		}
	}
	return ""
}

// handlerReason judges `|| H` after an assignment (exitOnly false) or after the chain inside a substitution (exitOnly true). A closed
// whitelist, the same one for the top level, function bodies and substitutions:
//
//	plain   exit | return | exit $? | return $? | exit N | return N      directly after the `||`, so `$?` is the failed command's status
//	brace   { …; exit N; }  { …; return N; }                              the LAST statement, N a literal 1..255; no bare, no `$?`, no `$var`
//	rc      NAME=$?  (top level only: the status is kept, scripts/check-exit-swallow.sh's rcbranch rule judges what is done with it)
//
// `return` is not allowed inside a substitution (it would only leave the substitution). The handler is a command by itself: a `&`, a
// pipe, a `&&` or a redirection on it moves it out of the foreground or can skip it.
func handlerReason(h *syntax.Stmt, exitOnly, allowRC bool) string {
	if h.Negated || h.Background || h.Coprocess || len(h.Redirs) > 0 {
		return "the handler is negated, in the background, a coprocess or has a redirection (a failed redirection skips it)"
	}
	switch cmd := h.Cmd.(type) {
	case *syntax.CallExpr:
		if allowRC && isRCAssign(cmd) {
			return ""
		}
		if why := plainExit(cmd, exitOnly); why != "" {
			return why
		}
		return ""
	case *syntax.Block:
		return braceReason(cmd, exitOnly)
	}
	return "the handler is not `exit` / `return` [N], `{ …; exit N; }` or NAME=$?"
}

func plainExit(ce *syntax.CallExpr, exitOnly bool) string {
	verb, ok := exitVerb(ce, exitOnly)
	if !ok {
		return "the handler is not `exit` / `return` [N], `{ …; exit N; }` or NAME=$?"
	}
	_ = verb
	switch len(ce.Args) {
	case 1:
		return ""
	case 2:
		if isStatusParam(ce.Args[1]) || exitLiteral(ce.Args[1]) {
			return ""
		}
	}
	return "the argument of exit / return must be nothing, `$?` right after the `||`, or a literal 1..255"
}

func braceReason(b *syntax.Block, exitOnly bool) string {
	if len(b.Stmts) == 0 {
		return "an empty handler group"
	}
	for _, s := range b.Stmts[:len(b.Stmts)-1] {
		if why := messageReason(s); why != "" {
			return why
		}
	}
	last := b.Stmts[len(b.Stmts)-1]
	ce, ok := last.Cmd.(*syntax.CallExpr)
	if !ok || last.Negated || last.Background || last.Coprocess || len(last.Redirs) > 0 {
		return "the last statement of the handler group must be `exit N` / `return N`"
	}
	if _, ok := exitVerb(ce, exitOnly); !ok || len(ce.Args) != 2 || !exitLiteral(ce.Args[1]) {
		return "the last statement of the handler group must be `exit N` / `return N` with a literal N of 1..255"
	}
	return ""
}

// messageReason: a statement in front of the closing exit is a message and nothing else: a simple command whose name is the literal `echo` or `printf`,
// no prefix assignment, no wrapper, and no command / process substitution in an argument or in a redirection. A function call (fail, warn, log…) is
// not that: what it does cannot be read here. This is a whitelist; nothing about `exit`, `return`, `eval`, `command` or their options is parsed.
func messageReason(s *syntax.Stmt) string {
	const why = "inside a handler group, every statement before the closing exit must be an echo or printf (literal name, no substitution in arguments or redirections)"
	ce, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || s.Negated || s.Background || s.Coprocess || len(ce.Assigns) != 0 || len(ce.Args) == 0 {
		return why
	}
	if name := litOf(ce.Args[0]); name != "echo" && name != "printf" {
		return why
	}
	for _, a := range ce.Args {
		if hasSubst(a) {
			return why
		}
	}
	for _, r := range s.Redirs {
		if hasSubst(r) {
			return why
		}
	}
	return ""
}

func exitVerb(ce *syntax.CallExpr, exitOnly bool) (string, bool) {
	if len(ce.Assigns) != 0 || len(ce.Args) == 0 {
		return "", false
	}
	switch name := litOf(ce.Args[0]); {
	case name == "exit", name == "return" && !exitOnly:
		return name, true
	}
	return "", false
}

// isRCAssign: `NAME=$?` and nothing else (the status is recorded, rcbranch checks the branch that reads it).
func isRCAssign(ce *syntax.CallExpr) bool {
	if len(ce.Args) != 0 || len(ce.Assigns) != 1 {
		return false
	}
	a := ce.Assigns[0]
	if a.Name == nil || a.Append || a.Naked || a.Index != nil || a.Array != nil || a.Value == nil || len(a.Value.Parts) != 1 {
		return false
	}
	p, ok := a.Value.Parts[0].(*syntax.ParamExp)
	return ok && isPlainStatus(p)
}

func isPlainStatus(p *syntax.ParamExp) bool {
	return p.Short && p.Param != nil && p.Param.Value == "?" && !p.Excl && !p.Length && !p.Width && p.Exp == nil && p.Slice == nil && p.Repl == nil && p.Index == nil
}

// isStatusParam: `$?` or "$?" (and nothing else in the word).
func isStatusParam(w *syntax.Word) bool {
	if len(w.Parts) != 1 {
		return false
	}
	switch p := w.Parts[0].(type) {
	case *syntax.ParamExp:
		return isPlainStatus(p)
	case *syntax.DblQuoted:
		if len(p.Parts) == 1 && !p.Dollar {
			pe, ok := p.Parts[0].(*syntax.ParamExp)
			return ok && isPlainStatus(pe)
		}
	}
	return false
}

// exitLiteral: a literal 1..255 (an exit status is taken modulo 256: 256 and 512 are 0), plainly written or in double quotes.
func exitLiteral(w *syntax.Word) bool {
	s := litOf(w)
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 255 && strconv.Itoa(n) == s
}

// litOf is the text of a word that is one literal (plain, or inside quotes with nothing to expand), else "".
func litOf(w *syntax.Word) string {
	if w == nil || len(w.Parts) != 1 {
		return ""
	}
	switch p := w.Parts[0].(type) {
	case *syntax.Lit:
		return p.Value
	case *syntax.SglQuoted:
		if !p.Dollar {
			return p.Value
		}
	case *syntax.DblQuoted:
		if len(p.Parts) == 1 && !p.Dollar {
			if l, ok := p.Parts[0].(*syntax.Lit); ok {
				return l.Value
			}
		}
	}
	return ""
}

func firstLit(ce *syntax.CallExpr) string {
	if len(ce.Args) == 0 {
		return ""
	}
	return litOf(ce.Args[0])
}

// isLabel: the label idiom `[ test ] && echo a || echo b` (test: `[`, `[[` or `test`; a and b: echo / printf of plain words), whose
// test holds no substitution. It cannot turn a failed command into a value: the value is always a or b.
func isLabel(cs *syntax.CmdSubst) bool {
	if len(cs.Stmts) != 1 {
		return false
	}
	s := cs.Stmts[0]
	or, ok := s.Cmd.(*syntax.BinaryCmd)
	if !ok || s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 || or.Op != syntax.OrStmt {
		return false
	}
	and, ok := or.X.Cmd.(*syntax.BinaryCmd)
	if !ok || or.X.Negated || or.X.Background || or.X.Coprocess || len(or.X.Redirs) > 0 || and.Op != syntax.AndStmt {
		return false
	}
	return isTest(and.X) && isEcho(and.Y) && isEcho(or.Y)
}

func isTest(s *syntax.Stmt) bool {
	if s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 {
		return false
	}
	switch t := s.Cmd.(type) {
	case *syntax.TestClause:
		return !hasSubst(t)
	case *syntax.CallExpr:
		name := firstLit(t)
		return len(t.Assigns) == 0 && (name == "[" || name == "test") && !hasSubst(t)
	}
	return false
}

func isEcho(s *syntax.Stmt) bool {
	ce, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 || len(ce.Assigns) != 0 {
		return false
	}
	name := firstLit(ce)
	return (name == "echo" || name == "printf") && !hasSubst(ce)
}

// hasSubst: a command substitution, a backquote or a process substitution anywhere under n.
func hasSubst(n syntax.Node) bool {
	found := false
	syntax.Walk(n, func(x syntax.Node) bool {
		switch x.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			found = true
		}
		return !found
	})
	return found
}

// contentReason: what may stand inside a (non-label) substitution — ONE `&&` chain of pipelines of simple commands, optionally
// followed by one `|| exit [N]` handler. No `;` or second statement, no `!`, no `&`, no subshell, group, `if` / `case` / loop / function
// / `[[`, no eval, `.` or source.
func contentReason(cs *syntax.CmdSubst) string {
	if len(cs.Stmts) != 1 {
		return "the substitution holds more than one statement: a later one hides the failure of the first"
	}
	s := cs.Stmts[0]
	if s.Negated || s.Background || s.Coprocess {
		return "the substitution's command is negated, in the background or a coprocess"
	}
	if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && b.Op == syntax.OrStmt {
		if why := chainReason(b.X); why != "" {
			return why
		}
		return handlerReason(b.Y, true, false)
	}
	return chainReason(s)
}

func chainReason(s *syntax.Stmt) string {
	if s.Negated || s.Background || s.Coprocess {
		return "a negated, background or coprocess command inside the substitution"
	}
	switch cmd := s.Cmd.(type) {
	case *syntax.CallExpr:
		return simpleReason(cmd)
	case *syntax.BinaryCmd:
		switch cmd.Op {
		case syntax.AndStmt, syntax.Pipe, syntax.PipeAll:
			if why := chainReason(cmd.X); why != "" {
				return why
			}
			return chainReason(cmd.Y)
		}
		return "a `||` inside the substitution other than the one handler that ends it"
	}
	return "the substitution holds a compound command (subshell, group, if, case, loop, function, [[ ]]…): only simple commands joined by `&&` or `|`"
}

func simpleReason(ce *syntax.CallExpr) string {
	if len(ce.Args) == 0 {
		return "" // only assignments
	}
	name := litOf(ce.Args[0])
	if name == "" {
		return "the command word is computed ($x, ${x}, \"$x\", a word with a substitution): what runs cannot be read"
	}
	switch name {
	case "command", "builtin", "exec", "eval", ".", "source":
		return "command, builtin, exec, eval, `.` and source run what follows them; this check does not read their options or their text"
	}
	return ""
}

// isSourceException is the one node that is exempt (kind scripts: check-kubeconfig accepts exactly this line):
// `. "$(dirname "$0")/../../../scripts/best-effort.sh"`. The command is `.`, it has one argument, and that word is exactly the
// substitution `dirname "$0"` followed by the literal path: any other path, command or extra argument is not this node.
func (c *checker) isSourceException() bool {
	n := len(c.stack)
	dq, ok := c.at(n - 1).(*syntax.DblQuoted)
	if !ok {
		return false
	}
	w, ok := c.at(n - 2).(*syntax.Word)
	if !ok || len(w.Parts) != 1 {
		return false
	}
	ce, ok := c.at(n - 3).(*syntax.CallExpr)
	if !ok || len(ce.Assigns) != 0 || len(ce.Args) != 2 || ce.Args[1] != w || litOf(ce.Args[0]) != "." {
		return false
	}
	if st, ok := c.at(n - 4).(*syntax.Stmt); !ok || st.Negated || st.Background || st.Coprocess || len(st.Redirs) > 0 {
		return false
	}
	return sourceExceptionShape(dq)
}

func sourceExceptionShape(dq *syntax.DblQuoted) bool {
	if dq.Dollar || len(dq.Parts) != 2 {
		return false
	}
	cs, ok := dq.Parts[0].(*syntax.CmdSubst)
	if !ok || cs.Backquotes || len(cs.Stmts) != 1 {
		return false
	}
	if l, ok := dq.Parts[1].(*syntax.Lit); !ok || l.Value != "/../../../scripts/best-effort.sh" {
		return false
	}
	s := cs.Stmts[0]
	ce, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 || len(ce.Assigns) != 0 || len(ce.Args) != 2 || litOf(ce.Args[0]) != "dirname" {
		return false
	}
	arg := ce.Args[1]
	if len(arg.Parts) != 1 {
		return false
	}
	q, ok := arg.Parts[0].(*syntax.DblQuoted)
	if !ok || q.Dollar || len(q.Parts) != 1 {
		return false
	}
	p, ok := q.Parts[0].(*syntax.ParamExp)
	return ok && p.Short && p.Param != nil && p.Param.Value == "0" && !p.Excl && !p.Length && !p.Width && p.Exp == nil && p.Slice == nil && p.Repl == nil && p.Index == nil
}

// errexitInfo is what the whole file says about errexit (set -e).
type errexitInfo struct {
	enabledAt uint // byte offset of the first top-level `set -e` / `set -o errexit` (valid when enabled)
	enabled   bool
	disabled  bool // anything anywhere that may turn it off: `set +e`, `set +o errexit`, `shopt -u …`, a `set` with a computed argument
}

func scanErrexit(f *syntax.File) errexitInfo {
	var ei errexitInfo
	top := map[*syntax.CallExpr]bool{}
	for _, st := range f.Stmts {
		if ce, ok := st.Cmd.(*syntax.CallExpr); ok {
			top[ce] = true
		}
	}
	syntax.Walk(f, func(n syntax.Node) bool {
		ce, ok := n.(*syntax.CallExpr)
		if !ok || len(ce.Args) == 0 {
			return true
		}
		switch litOf(ce.Args[0]) {
		case "shopt":
			for _, a := range ce.Args[1:] {
				if l := litOf(a); strings.HasPrefix(l, "-") && strings.Contains(l, "u") {
					ei.disabled = true
				}
			}
		case "set":
			on, off := setFlags(ce.Args[1:])
			if off {
				ei.disabled = true
			}
			if on && top[ce] && !ei.enabled {
				ei.enabled, ei.enabledAt = true, ce.Pos().Offset()
			}
		}
		return true
	})
	return ei
}

// setFlags reads the arguments of `set`: on = this turns errexit on (-e, -eu, -euo pipefail, -o errexit); off = it may turn it off (+e, +o errexit, a computed argument).
func setFlags(args []*syntax.Word) (on, off bool) {
	for i := 0; i < len(args); i++ {
		l := litOf(args[i])
		switch {
		case l == "":
			return on, true // `set "$x"`: unknowable
		case l == "--":
			return on, off
		case l == "-o" || l == "+o":
			if i+1 < len(args) {
				i++
				if litOf(args[i]) == "errexit" {
					if l == "-o" {
						on = true
					} else {
						off = true
					}
				} else if litOf(args[i]) == "" {
					off = true
				}
			}
		case strings.HasPrefix(l, "-") && strings.Contains(l, "e") && !strings.HasPrefix(l, "--"):
			on = true
		case strings.HasPrefix(l, "+") && strings.Contains(l, "e"):
			off = true
		}
	}
	return on, off
}

// errexitReason: a top-level assignment with no handler relies on `set -e` to stop the script when its substitution fails. That holds only when
//
//	(1) the file turns errexit on at top level BEFORE this statement and nothing in the file may turn it off, and
//	(2) every ancestor of the statement is a place where errexit is in force: the file's statement list, the body of an `if` / `elif` / `else`, of a loop
//	    (`do`), of a `case` branch, of a `{ … }` or `( … )`; and each compound command on the way is a whole statement: not part of a condition, not the
//	    left of `&&` / `||`, not negated, not in the background, not a pipeline operand, not inside a substitution. Any other ancestor type is not trusted.
func (c *checker) errexitReason(si int) string {
	st := c.at(si).(*syntax.Stmt)
	if !c.eo.enabled || c.eo.disabled || st.Pos().Offset() < c.eo.enabledAt {
		return "a plain assignment relies on `set -e`, which this file does not provably have in force here (no top-level `set -e` before it, or a `set +e` / `shopt -u` / computed `set` anywhere): add `|| exit N` or use the condition form"
	}
	const lost = "errexit does not apply where this assignment stands (a condition, the left of `&&` / `||`, a negation, the background, a pipeline, a substitution…): add `|| exit N` or use the condition form"
	i := si
	for {
		n := c.at(i)
		if s, ok := n.(*syntax.Stmt); ok && (s.Negated || s.Background || s.Coprocess) {
			return lost
		}
		if i == 0 {
			return ""
		}
		switch p := c.at(i - 1).(type) {
		case *syntax.File:
			return ""
		case *syntax.Stmt: // n is the command of that statement
			i--
		case *syntax.BinaryCmd:
			s, ok := n.(*syntax.Stmt)
			if !ok || (p.Op != syntax.AndStmt && p.Op != syntax.OrStmt) || p.Y != s {
				return lost
			}
			i--
		case *syntax.Block, *syntax.Subshell, *syntax.CaseItem, *syntax.CaseClause:
			i--
		case *syntax.IfClause:
			if s, ok := n.(*syntax.Stmt); ok && !containsStmt(p.Then, s) {
				return lost // a condition
			}
			i--
		case *syntax.WhileClause:
			if s, ok := n.(*syntax.Stmt); ok && !containsStmt(p.Do, s) {
				return lost
			}
			i--
		case *syntax.ForClause:
			if s, ok := n.(*syntax.Stmt); ok && !containsStmt(p.Do, s) {
				return lost
			}
			i--
		default:
			return lost
		}
	}
}
