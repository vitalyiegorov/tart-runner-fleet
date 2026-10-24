#!/bin/sh
# Run while building a disposable runner image, never in an active job.
set -eu
version=${1:?exact Node version required}
arch=${2:?Node architecture required}
cache=${3:?runner tool cache root required}
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || exit 2
case "$arch" in x64|arm64) ;; *) exit 2 ;; esac
case "$cache" in /*) ;; *) exit 2 ;; esac

destination="$cache/$version/$arch"
marker="$cache/$version/$arch.complete"
if [ -f "$marker" ] && [ -x "$destination/bin/node" ] &&
    [ "$("$destination/bin/node" --version)" = "v$version" ]; then
    exit 0
fi

mkdir -p "$cache/$version"
scratch=$(mktemp -d "$cache/$version/.install-XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
asset="node-v$version-linux-$arch.tar.xz"
base="https://nodejs.org/dist/v$version"
curl --fail --silent --show-error --location --retry 3 --output "$scratch/$asset" "$base/$asset"
curl --fail --silent --show-error --location --retry 3 --output "$scratch/SHASUMS256.txt" "$base/SHASUMS256.txt"
awk -v asset="$asset" '$2 == asset { print; found=1 } END { if (!found) exit 1 }' \
    "$scratch/SHASUMS256.txt" > "$scratch/checksum"
(cd "$scratch" && sha256sum --check checksum)
mkdir "$scratch/extracted"
tar -xJf "$scratch/$asset" --strip-components=1 -C "$scratch/extracted"
test "$("$scratch/extracted/bin/node" --version)" = "v$version"
rm -f "$marker"
rm -rf "$destination"
mv "$scratch/extracted" "$destination"
touch "$marker"
