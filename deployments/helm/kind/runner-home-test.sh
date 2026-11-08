# Sourced by the drills that run kind/*.sh directly against fake tools (negative-test.sh): they stand in for the
# scratch HOME that run.sh gives a script. The scripts refuse to start without it (#444). Usage: . runner-home-test.sh <dir>
# The container engine's own config stays where it was.
_real_home=${HOME:-}
export XDG_CONFIG_HOME=${XDG_CONFIG_HOME:-$_real_home/.config} XDG_DATA_HOME=${XDG_DATA_HOME:-$_real_home/.local/share}
[ -d "$_real_home/.docker" ] && export DOCKER_CONFIG=${DOCKER_CONFIG:-$_real_home/.docker}
mkdir -p "$1"
: > "$1/.kind-runner"
export ENGINE_HOME="$_real_home"  # the caller's HOME, for the one command that pulls an image (credential helpers read it)
export HOME="$1" KIND_RUNNER_HOME="$1"
