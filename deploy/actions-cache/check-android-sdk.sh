#!/bin/sh
# Reads android-sdk-packages.txt on stdin. Fails unless sdkmanager, run offline at
# ANDROID_SDK_ROOT, lists every exact id and a licence is accepted. Nothing here
# installs: Containerfile.local copies the packages in from the node's own files.
set -eu
root=${ANDROID_SDK_ROOT:?}
packages=$(sed 's/#.*//' | awk 'NF == 0 { next }
  NF != 1 || $1 !~ /^[a-z][a-z-]*(;[A-Za-z0-9._-]+)+$/ { bad = 1 }
  { n++; print } END { exit bad || n == 0 }')
installed=$("$root/cmdline-tools/latest/bin/sdkmanager" --sdk_root="$root" --list_installed 2>/dev/null |
    awk 'NF >= 2 { print $1 }')
missing=$(printf '%s\n' "$packages" | while read -r package; do
    grep -Fxq -e "$(printf '%s' "$package" | tr ';' /)" <<EOL || printf ' %s' "$package"
$installed
EOL
done)
[ -z "$missing" ] || { echo "android sdk packages missing:$missing" >&2; exit 1; }
[ -s "$root/licenses/android-sdk-license" ] || { echo 'android sdk licences not accepted' >&2; exit 1; }
