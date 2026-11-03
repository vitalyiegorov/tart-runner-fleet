#!/bin/sh
# Reads runner-tools.txt on stdin. Installs its packages, then fails unless
# every listed command resolves on the PATH the daemon gives the runner.
# --check skips the install, to audit a built image. RUNNER_PATH exists for
# the tests; an image build never sets it.
set -eu
case "${1:-install}" in install | --check) mode=${1:-install} ;; *) exit 2 ;; esac
tools=$(sed 's/#.*//' | awk 'NF == 0 { next }
  NF != 2 || $1 !~ /^[a-z0-9][a-z0-9.+-]*$/ || $2 !~ /^[a-z0-9][a-z0-9.+-]*$/ { bad = 1 }
  { n++; print } END { exit bad || n == 0 }')
if [ "$mode" = install ]; then
    apt-get update
    # shellcheck disable=SC2046 # package names are validated above and split on purpose
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        $(printf '%s\n' "$tools" | awk '{ print $2 }' | sort -u)
fi
missing=$(printf '%s\n' "$tools" | while read -r command package; do
    (PATH=${RUNNER_PATH:-/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin} && command -v "$command") >/dev/null ||
        printf ' %s(%s)' "$command" "$package"
done)
[ -z "$missing" ] || { echo "runner tools missing:$missing" >&2; exit 1; }
