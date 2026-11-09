# The bounded contexts of a `go work edit -json` document, one name per line: every go.work module under pkgs/ except
# kernel and platform (the same rule as pkgs/platform/archtest/imports.go, which decides what a BC is for the import
# rules). Taskfile.yml's BCS runs this, so go.work is the only list of BCs. A name is spliced into shell commands and
# paths by the gen-openapi loop, so it is an allowlist, ^[a-z][a-z0-9]*$: anything else (a nested pkgs/a/b module, an
# uppercase or dashed name) fails with the path JSON-quoted rather than being skipped. No BC at all is a valid skeleton.
(.Use // [])[]
| .DiskPath as $p
| ($p | ltrimstr("./")) as $m
| select($m | startswith("pkgs/"))
| ($m | ltrimstr("pkgs/")) as $bc
| select(($bc == "kernel" or $bc == "platform") | not)
| if ($bc | test("^[a-z][a-z0-9]*$")) then $bc
  else error("go.work module path " + ($p | @json) + " is not a bounded context name ^[a-z][a-z0-9]*$ (pkgs/<bc>)")
  end
