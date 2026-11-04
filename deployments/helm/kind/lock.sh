# BASH_ENV of every command run.sh starts: bash reads it before the first line of a script it runs, in every nested bash too.
# Read-only means a script (or a bash -c under it) cannot unset these, assign them, `read -r` into them, `printf -v` them, `declare`
# or `export HOME=...` them: the shell refuses ("readonly variable") and the value stays what run.sh set. (`export -n` still removes a
# variable from the environment of children; that leaves them with no KUBECONFIG and an HOME with no .kube, which fails safe.)
readonly KUBECONFIG KIND_KUBECONFIG PATH HOME BASH_ENV KIND_RUNNER_HOME
