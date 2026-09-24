#!/bin/sh
# Download a specific public release. Provisioning lives only in the verified helper.
set -eu
umask 077

fail() { printf '%s\n' "garm-orbstack: $*" >&2; exit 1; }
version=
confirm=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            [ "$#" -ge 2 ] || fail '--version requires a value'
            [ -z "$version" ] || fail 'duplicate --version'
            version=$2
            shift 2
            ;;
        --confirm-version-change)
            [ -z "$confirm" ] || fail 'duplicate --confirm-version-change'
            confirm=--confirm-version-change
            shift
            ;;
        *) fail 'usage: sh install.sh --version vX.Y.Z [--confirm-version-change]' ;;
    esac
done
[ -n "$version" ] || fail 'an explicit --version vX.Y.Z is required'
case "$version" in v*) ;; *) fail 'version must be vX.Y.Z' ;; esac
numbers=${version#v}
case "$numbers" in ''|*[!0-9.]*|.*|*.|*..*) fail 'version must be vX.Y.Z' ;; esac
saved_ifs=$IFS
IFS=.
set -- $numbers
IFS=$saved_ifs
[ "$#" -eq 3 ] || fail 'version must be vX.Y.Z'
[ "$(uname -s)" = Darwin ] || fail 'only macOS is supported'
case "$(uname -m)" in
    arm64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) fail 'only Darwin arm64 and amd64 are supported' ;;
esac

stage=$(mktemp -d "${TMPDIR:-/tmp}/garm-orbstack.XXXXXXXX")
trap 'rm -rf "$stage"' 0
trap 'exit 1' HUP INT TERM
base="https://github.com/mainuli/garm-provider-orbstack/releases/download/$version"
assets="garm-orbstack_${version}_darwin_${arch} garm-provider-orbstack_${version}_darwin_${arch} garm_${version}_darwin_${arch} garm-cli_${version}_darwin_${arch} release-lock.json"
for asset in $assets SHA256SUMS; do
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --location --silent --show-error --output "$stage/$asset" "$base/$asset" || fail "download failed: $asset"
done

for asset in $assets; do
    count=0
    expected=
    while IFS=' ' read -r digest name extra || [ -n "${digest}${name}${extra}" ]; do
        name=${name#\*}
        if [ "$name" = "$asset" ]; then
            [ -z "$extra" ] || fail "malformed checksum entry: $asset"
            count=$((count + 1))
            expected=$digest
        fi
    done < "$stage/SHA256SUMS"
    [ "$count" -eq 1 ] || fail "expected exactly one checksum entry: $asset"
    [ "${#expected}" -eq 64 ] || fail "invalid SHA-256 digest: $asset"
    case "$expected" in *[!0-9a-fA-F]*) fail "invalid SHA-256 digest: $asset" ;; esac
    expected=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
    actual=$(shasum -a 256 "$stage/$asset") || fail "checksum failed: $asset"
    actual=${actual%% *}
    [ "$actual" = "$expected" ] || fail "checksum mismatch: $asset"
done

for binary in garm-orbstack garm-provider-orbstack garm garm-cli; do
    chmod 755 "$stage/${binary}_${version}_darwin_${arch}"
done
helper="$stage/garm-orbstack_${version}_darwin_${arch}"
actual_version=$("$helper" version) || fail 'verified helper could not report its version'
[ "$actual_version" = "$version" ] || fail 'helper version differs from requested release'
if [ -n "$confirm" ]; then
    "$helper" install --from-dir "$stage" "$confirm"
else
    "$helper" install --from-dir "$stage"
fi
