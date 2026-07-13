#!/bin/sh
set -eu

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

for pass in one two; do
	pass_dir="$temporary/$pass"
	mkdir -p "$pass_dir"
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/fleetd" ./cmd/fleetd
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/fleetctl" ./cmd/fleetctl
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildvcs=true -ldflags="$ldflags" -o "$pass_dir/tart-runner-fleet-bootstrap" ./cmd/tart-runner-fleet-bootstrap
  ./scripts/run-tool.sh cyclonedx-gomod bin -json -std -noserial -notimestamp \
    -version "$version" -output "$pass_dir/fleetd.cdx.json" "$pass_dir/fleetd"
  ./scripts/run-tool.sh cyclonedx-gomod bin -json -std -noserial -notimestamp \
    -version "$version" -output "$pass_dir/fleetctl.cdx.json" "$pass_dir/fleetctl"
  ./scripts/run-tool.sh cyclonedx-gomod bin -json -std -noserial -notimestamp \
    -version "$version" -output "$pass_dir/tart-runner-fleet-bootstrap.cdx.json" "$pass_dir/tart-runner-fleet-bootstrap"
done
cmp "$temporary/one/fleetd" "$temporary/two/fleetd"
cmp "$temporary/one/fleetctl" "$temporary/two/fleetctl"
cmp "$temporary/one/tart-runner-fleet-bootstrap" "$temporary/two/tart-runner-fleet-bootstrap"
cmp "$temporary/one/fleetd.cdx.json" "$temporary/two/fleetd.cdx.json"
cmp "$temporary/one/fleetctl.cdx.json" "$temporary/two/fleetctl.cdx.json"
cmp "$temporary/one/tart-runner-fleet-bootstrap.cdx.json" "$temporary/two/tart-runner-fleet-bootstrap.cdx.json"

cp "$temporary/one/fleetd" "$staging/fleetd"
cp "$temporary/one/fleetctl" "$staging/fleetctl"
cp "$temporary/one/tart-runner-fleet-bootstrap" "$staging/tart-runner-fleet-bootstrap"
cp "$temporary/one/fleetd.cdx.json" "$staging/fleetd.cdx.json"
cp "$temporary/one/fleetctl.cdx.json" "$staging/fleetctl.cdx.json"
cp "$temporary/one/tart-runner-fleet-bootstrap.cdx.json" "$staging/tart-runner-fleet-bootstrap.cdx.json"
(cd "$staging" && go version -m fleetd) > "$staging/BUILDINFO.txt"
printf '%s\n' "$version" > "$staging/RELEASE_VERSION"

archive="tart-runner-fleet-$version-darwin-arm64.tar.gz"
for pass in one two; do
  tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner \
    -czf "$temporary/$archive.$pass" -C "$staging" \
    fleetd fleetctl tart-runner-fleet-bootstrap \
    fleetd.cdx.json fleetctl.cdx.json tart-runner-fleet-bootstrap.cdx.json \
    BUILDINFO.txt RELEASE_VERSION
done
cmp "$temporary/$archive.one" "$temporary/$archive.two"
cp "$temporary/$archive.one" "$staging/$archive"
(cd "$staging" && shasum -a 256 \
  "$archive" fleetd fleetctl tart-runner-fleet-bootstrap \
  fleetd.cdx.json fleetctl.cdx.json tart-runner-fleet-bootstrap.cdx.json \
  BUILDINFO.txt RELEASE_VERSION > SHA256SUMS)
rmdir "$output"
mv "$staging" "$output"
staging=""
printf 'built reproducible release %s\n' "$output/$archive"
