#!/bin/sh
# Run while building a disposable runner image, never in an active job.
# Lays out the CodeQL bundle exactly as github/codeql-action caches it, so
# init finds it in RUNNER_TOOL_CACHE instead of downloading ~560 MB per job.
set -eu
version=${1:?exact CodeQL CLI version required}
cache=${2:?runner tool cache root required}
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || exit 2
case "$cache" in /*) ;; *) exit 2 ;; esac

destination="$cache/$version/x64"
marker="$cache/$version/x64.complete"
if [ -f "$marker" ] && [ -x "$destination/codeql/codeql" ] &&
    "$destination/codeql/codeql" version --format=terse | grep -qx "$version"; then
    exit 0
fi

mkdir -p "$cache/$version"
scratch=$(mktemp -d "$cache/$version/.install-XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
asset=codeql-bundle-linux64.tar.zst
base="https://github.com/github/codeql-action/releases/download/codeql-bundle-v$version"
curl --fail --silent --show-error --location --retry 3 --output "$scratch/$asset" "$base/$asset"
curl --fail --silent --show-error --location --retry 3 --output "$scratch/checksum" "$base/$asset.checksum.txt"
awk -v asset="$asset" '$2 == asset { found=1 } END { if (!found) exit 1 }' "$scratch/checksum"
(cd "$scratch" && sha256sum --check checksum)
mkdir "$scratch/extracted"
tar --zstd -xf "$scratch/$asset" -C "$scratch/extracted"
"$scratch/extracted/codeql/codeql" version --format=terse | grep -qx "$version"
rm -f "$marker"
rm -rf "$destination"
mv "$scratch/extracted" "$destination"
touch "$marker"
