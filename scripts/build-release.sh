#!/bin/sh
set -eu

# Builds one immutable release. ADR 0034 gives the fleet two node types, so a
# release carries two archives: darwin/arm64 for the Apple nodes and linux/amd64
# for the GEEKOM node. They are built from the same source, twice each, byte
# compared, and listed in one SHA256SUMS — the linux archive therefore has
# exactly the integrity properties the darwin one has always had, and neither can
# be published without the other having reproduced.
#
# Each archive is a complete generation for its node: the `fleet` executable for
# that platform, its SBOM, its build identity, the guest bootstrap helpers, the
# service definitions that boot it, and the renderer that turns them into a
# running service.

version="${1:-}"
output="${2:-dist}"
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
if ! printf '%s\n' "$version" | grep -E -q "$semver"; then
  printf 'version must be immutable semver beginning with v (for example v1.2.3)\n' >&2
  exit 2
fi
case "$output" in ''|/) printf 'unsafe output directory\n' >&2; exit 2 ;; esac

mkdir -p "$output"
if find "$output" -mindepth 1 -print -quit | grep -q .; then
  printf 'output directory must be empty: %s\n' "$output" >&2
  exit 2
fi

temporary="$(mktemp -d)"
staging="$(mktemp -d "${output}.staging.XXXXXX")"
cleanup() {
  rm -rf "$temporary"
  if [ -n "$staging" ]; then
    rm -rf "$staging"
  fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
export SOURCE_DATE_EPOCH=0
ldflags="-s -w -X main.version=$version"

# The reproducible artefacts, in the names they carry as loose release assets.
# `fleet` without a suffix is the darwin/arm64 controller, which is the name
# every installed macOS generation and every operator runbook already uses.
reproducible='fleet
fleet-linux-amd64
tart-runner-fleet-bootstrap-darwin-arm64
tart-runner-fleet-bootstrap-linux-arm64
tart-runner-fleet-bootstrap-linux-amd64
fleet.cdx.json
fleet-linux-amd64.cdx.json
tart-runner-fleet-bootstrap-darwin-arm64.cdx.json
tart-runner-fleet-bootstrap-linux-arm64.cdx.json
tart-runner-fleet-bootstrap-linux-amd64.cdx.json'

launchd_files='com.vitalyiegorov.tart-runner-fleet.plist
com.vitalyiegorov.tart-runner-fleet.shadow.plist
com.vitalyiegorov.tart-runner-fleet.canary.plist
com.vitalyiegorov.tart-runner-fleet.authority.plist
render-launchd.sh'

systemd_files='tart-runner-fleet.service
tart-runner-fleet-shadow.service
tart-runner-fleet-canary.service
tart-runner-fleet-authority.service
tart-runner-fleet-updater.service
tart-runner-fleet-updater.timer
tart-runner-fleet-updater-handoff.service
render-systemd.sh'

for pass in one two; do
  pass_dir="$temporary/$pass"
  mkdir -p "$pass_dir"
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/fleet" ./cmd/fleet
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/fleet-linux-amd64" ./cmd/fleet
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/tart-runner-fleet-bootstrap-darwin-arm64" ./cmd/tart-runner-fleet-bootstrap
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/tart-runner-fleet-bootstrap-linux-arm64" ./cmd/tart-runner-fleet-bootstrap
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/tart-runner-fleet-bootstrap-linux-amd64" ./cmd/tart-runner-fleet-bootstrap
  for binary in fleet fleet-linux-amd64 tart-runner-fleet-bootstrap-darwin-arm64 tart-runner-fleet-bootstrap-linux-arm64 tart-runner-fleet-bootstrap-linux-amd64; do
    ./scripts/run-tool.sh cyclonedx-gomod bin -json -std -noserial -notimestamp \
      -version "$version" -output "$pass_dir/$binary.cdx.json" "$pass_dir/$binary"
  done
done
printf '%s\n' "$reproducible" | while IFS= read -r artefact; do
  cmp "$temporary/one/$artefact" "$temporary/two/$artefact"
done

printf '%s\n' "$reproducible" | while IFS= read -r artefact; do
  cp "$temporary/one/$artefact" "$staging/$artefact"
done
printf '%s\n' "$launchd_files" | while IFS= read -r file; do
  cp "launchd/$file" "$staging/$file"
done
printf '%s\n' "$systemd_files" | while IFS= read -r file; do
  cp "systemd/$file" "$staging/$file"
done
(cd "$staging" && go version -m fleet) > "$staging/BUILDINFO.txt"
(cd "$staging" && go version -m fleet-linux-amd64) > "$staging/BUILDINFO-linux-amd64.txt"
printf '%s\n' "$version" > "$staging/RELEASE_VERSION"

# archive assembles one node type's generation under the member names it must
# carry inside the archive: every node unpacks its controller as `fleet`, so the
# platform suffix exists only among the loose assets, where both must coexist.
archive() {
  goos_arch="$1"
  controller="$2"
  buildinfo="$3"
  definitions="$4"
  assembly="$temporary/assembly-$goos_arch"
  mkdir -p "$assembly"
  cp "$staging/$controller" "$assembly/fleet"
  cp "$staging/$controller.cdx.json" "$assembly/fleet.cdx.json"
  cp "$staging/$buildinfo" "$assembly/BUILDINFO.txt"
  cp "$staging/RELEASE_VERSION" "$assembly/RELEASE_VERSION"
  for shared in tart-runner-fleet-bootstrap-darwin-arm64 tart-runner-fleet-bootstrap-linux-arm64 \
    tart-runner-fleet-bootstrap-linux-amd64 tart-runner-fleet-bootstrap-darwin-arm64.cdx.json \
    tart-runner-fleet-bootstrap-linux-arm64.cdx.json tart-runner-fleet-bootstrap-linux-amd64.cdx.json; do
    cp "$staging/$shared" "$assembly/$shared"
  done
  members='fleet
fleet.cdx.json
tart-runner-fleet-bootstrap-darwin-arm64
tart-runner-fleet-bootstrap-linux-arm64
tart-runner-fleet-bootstrap-linux-amd64
tart-runner-fleet-bootstrap-darwin-arm64.cdx.json
tart-runner-fleet-bootstrap-linux-arm64.cdx.json
tart-runner-fleet-bootstrap-linux-amd64.cdx.json
BUILDINFO.txt
RELEASE_VERSION'
  printf '%s\n' "$definitions" | while IFS= read -r file; do
    cp "$staging/$file" "$assembly/$file"
  done
  members="$members
$definitions"
  printf '%s\n' "$members" | LC_ALL=C sort > "$assembly/.members"
  name="tart-runner-fleet-$version-$goos_arch.tar.gz"
  for pass in one two; do
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
      -czf "$temporary/$name.$pass" -C "$assembly" -T "$assembly/.members"
  done
  cmp "$temporary/$name.one" "$temporary/$name.two"
  cp "$temporary/$name.one" "$staging/$name"
}

archive darwin-arm64 fleet BUILDINFO.txt "$launchd_files"
archive linux-amd64 fleet-linux-amd64 BUILDINFO-linux-amd64.txt "$systemd_files"

darwin_archive="tart-runner-fleet-$version-darwin-arm64.tar.gz"
linux_archive="tart-runner-fleet-$version-linux-amd64.tar.gz"
manifest="$(printf '%s\n%s\n%s\n%s\n%s\nBUILDINFO.txt\nBUILDINFO-linux-amd64.txt\nRELEASE_VERSION\n' \
  "$darwin_archive" "$linux_archive" "$reproducible" "$launchd_files" "$systemd_files")"
# shellcheck disable=SC2046 -- the manifest is a newline-separated list of names
# this script generated, and word splitting is how it becomes an argument vector.
(cd "$staging" && shasum -a 256 $(printf '%s\n' "$manifest") > SHA256SUMS)
rmdir "$output"
mv "$staging" "$output"
staging=""
printf 'built reproducible release %s and %s\n' "$output/$darwin_archive" "$output/$linux_archive"
