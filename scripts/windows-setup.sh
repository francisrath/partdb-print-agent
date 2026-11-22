#!/usr/bin/env bash
# Builds setup.exe from the Windows exes GoReleaser put in dist/ (.goreleaser-windows.yaml), with Inno
# Setup under Wine in Docker (packaging/windows/inno), and adds it to dist/checksums-windows.txt.
# Needs an x86-64 Docker host: Wine doesn't run under Rosetta on Apple silicon.
set -euo pipefail

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

docker build --platform linux/amd64 -t pdb-print-inno packaging/windows/inno
docker run --rm --platform linux/amd64 -v "$PWD/$work:/work" pdb-print-inno "/DVersion=$v" pdb-print.iss

mv "$work/$setup" dist/
(cd dist && sha256sum "$setup" >>checksums-windows.txt)
