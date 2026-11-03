# The module list of a `go work edit -json` document, one path per line without the "./" prefix.
# The one rule for what a go.work module path may be: Taskfile.yml's MODULES runs this, and scripts/check-modules.sh
# runs the same file, so there is no second copy of the rule (#413 r6). The paths are spliced into shell commands by
# the Taskfile's per-module loops, so the rule is an allowlist: [A-Za-z0-9._/-], not starting with "-". Anything else
# (whitespace, $, quotes, backticks, ;, &, *, newlines ...) fails with the path JSON-quoted, and so does an empty list
# (every per-module task would iterate nothing and pass).
(.Use // []) as $use
| if ($use | length) == 0 then error("go.work lists no modules") else $use[] end
| .DiskPath as $p
| ($p | ltrimstr("./")) as $m
| if ($m | test("^[A-Za-z0-9._][A-Za-z0-9._/-]*$")) then $m
  else error("go.work module path " + ($p | @json) + " has characters outside [A-Za-z0-9._/-] (or starts with -): MODULES cannot carry it")
  end
