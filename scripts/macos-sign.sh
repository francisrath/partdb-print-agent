#!/bin/sh
# Signs one macOS binary with the hardened runtime; .goreleaser-darwin.yaml runs it on each universal
# binary, before the archive, checksums and cask are made.
# With MACOS_SIGN_IDENTITY set (release.yml): Developer ID signature, notarisation, Gatekeeper check.
# Without it (snapshot builds, PR CI): ad-hoc signature only.
# Release env: MACOS_SIGN_IDENTITY, MACOS_SIGN_KEYCHAIN, MACOS_NOTARY_KEY_PATH (.p8 file),
# MACOS_NOTARY_KEY_ID, MACOS_NOTARY_ISSUER_ID.
set -eu
bin=$1

if [ -z "${MACOS_SIGN_IDENTITY:-}" ]; then
	codesign --force --options runtime --sign - "$bin"
	codesign --verify --strict "$bin"
	exit 0
fi

codesign --force --options runtime --timestamp --keychain "$MACOS_SIGN_KEYCHAIN" \
	--sign "$MACOS_SIGN_IDENTITY" "$bin"
codesign --verify --strict --verbose=2 "$bin"

# A bare binary can't be stapled: Gatekeeper looks the notarisation ticket up online on first run.
# Notarisation usually takes minutes; the first submissions of a new certificate took over 20.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
ditto -c -k --keepParent "$bin" "$tmp/bin.zip"
xcrun notarytool submit "$tmp/bin.zip" --key "$MACOS_NOTARY_KEY_PATH" --key-id "$MACOS_NOTARY_KEY_ID" \
	--issuer "$MACOS_NOTARY_ISSUER_ID" --wait --timeout 1h --output-format json >"$tmp/result.json"
status=$(plutil -extract status raw "$tmp/result.json")
if [ "$status" != Accepted ]; then
	echo "notarisation of $bin: $status" >&2
	xcrun notarytool log "$(plutil -extract id raw "$tmp/result.json")" --key "$MACOS_NOTARY_KEY_PATH" \
		--key-id "$MACOS_NOTARY_KEY_ID" --issuer "$MACOS_NOTARY_ISSUER_ID" >&2 || true
	exit 1
fi
spctl --assess --type install --verbose=2 "$bin"
