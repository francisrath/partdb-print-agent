#!/usr/bin/env bash
# Builds setup.exe from the Windows exes GoReleaser put in dist/ (.goreleaser-windows.yaml) with Inno
# Setup under Wine, and adds it to dist/checksums-windows.txt. Inno Setup is only a build tool here:
# nothing of it ends up in pdb-print. Needs Wine with its 32-bit side (Inno's setup refuses to run
# without it) and xvfb-run, on x86-64 Linux (Wine doesn't run under Rosetta); on Ubuntu:
#   sudo dpkg --add-architecture i386 && sudo apt-get update
#   sudo apt-get install -y --no-install-recommends wine wine64 wine32:i386 xvfb
set -euo pipefail

# The official installer, pinned by version and SHA-256 (the GitHub release's asset digest)
inno_version=7.1.1
inno_url=https://github.com/jrsoftware/issrc/releases/download/is-7_1_1/innosetup-7.1.1-x64.exe
inno_sha256=8a5a6711c3766a6e5fb74f280ba0e3551e719ce94538d901fdbd1f8f9920b3f0

export WINEDEBUG=-all WINEPREFIX="${XDG_CACHE_HOME:-$HOME/.cache}/pdb-print-inno-$inno_version"
# wineserver -w waits for Wine's background processes (the installer and ISCC already run to the end);
# Ubuntu keeps wineserver off PATH
wineserver=$(command -v wineserver || find /usr/lib /usr/libexec -name 'wineserver*' -type f 2>/dev/null | head -n1 || true)
wine_wait() { if [ -n "$wineserver" ]; then "$wineserver" -w; fi; }
iscc="$WINEPREFIX/drive_c/inno/ISCC.exe"
if [ ! -f "$iscc" ]; then
  rm -rf "$WINEPREFIX"
  mkdir -p "$WINEPREFIX"
  curl -fsSLo "$WINEPREFIX/is.exe" "$inno_url"
  echo "$inno_sha256  $WINEPREFIX/is.exe" | sha256sum -c -
  xvfb-run -a wine "$WINEPREFIX/is.exe" /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP- '/DIR=C:\inno' '/LOG=C:\is.log' || true
  wine_wait
  if [ ! -f "$iscc" ]; then
    echo "Inno Setup didn't install:" >&2
    cat "$WINEPREFIX/drive_c/is.log" >&2 || true
    exit 1
  fi
fi

v=$(jq -r .version dist/metadata.json)
setup="pdb-print_${v}_windows_amd64_setup.exe"
work=dist/setup
rm -rf "$work"
mkdir -p "$work"
for exe in pdb-print.exe pdb-printw.exe; do
  path=$(jq -r --arg n "$exe" '.[] | select(.type == "Binary" and .goos == "windows" and .name == $n) | .path' dist/artifacts.json)
  cp "$path" "$work/$exe"
done
cp packaging/windows/pdb-print.iss assets/icon/windows/pdb-print.ico "$work/"

(cd "$work" && xvfb-run -a wine "$iscc" "/DVersion=$v" pdb-print.iss)
wine_wait

mv "$work/$setup" dist/
(cd dist && sha256sum "$setup" >>checksums-windows.txt)
