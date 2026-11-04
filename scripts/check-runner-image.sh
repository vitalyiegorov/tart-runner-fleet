#!/bin/sh
# Smoke the actual image as its configured user, with no network or credentials.
set -eu
image=${1:?runner image required}
shift
podman run --rm --network=none --entrypoint sh "$image" -eu -c '
  test "$(id -u)" -ne 0
  test "${ACTIONS_RESULTS_URL:-}" = http://host.containers.internal:3017/
  work=/home/runner/actions-runner/_work
  mkdir -p "$work/_temp"
  test -w "$work/_temp"
  for version do
    case "$(uname -m)" in x86_64) arch=x64 ;; aarch64) arch=arm64 ;; *) exit 2 ;; esac
    tool="$work/_tool/node/$version/$arch"
    test -f "$tool.complete"
    test "$("$tool/bin/node" --version)" = "v$version"
  done
  for complete in "$work"/_tool/CodeQL/*/x64.complete; do
    version=${complete%/x64.complete}
    version=${version##*/}
    test "$("${complete%.complete}/codeql/codeql" version --format=terse)" = "$version"
  done
  sh /usr/local/libexec/trf-android-sdk/check-android-sdk.sh < /usr/local/libexec/trf-android-sdk/android-sdk-packages.txt
  printf "Runner user, temporary directory, endpoint, requested Node caches, baked CodeQL and Android SDK verified offline.\n"
' sh "$@"
