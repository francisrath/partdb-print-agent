#!/usr/bin/env bash
# gen-icons.sh – regenerate every app icon from the SVG sources. Output is committed.
#
# Sources:
#   docs/logo.svg                 full Labelbot logo (used for sizes >= 128 px)
#   assets/icon/mark.svg          simplified mark (sizes < 128 px, favicon)
#   assets/icon/tray-template.svg monochrome menu-bar/tray template
#   assets/icon/social-preview.svg  text/background for the GitHub social preview
#
# Dependencies (macOS):
#   brew install librsvg imagemagick   # rsvg-convert, magick (perl ships with macOS/Linux)
#   iconutil                           # ships with macOS (Xcode command line tools)
#   go                                 # runs go-winres for the Windows resource
# On Linux everything except the .icns step works; it is skipped with a warning.
set -euo pipefail

cd "$(dirname "$0")/.."
GO_WINRES=github.com/tc-hib/go-winres@v0.3.3

for tool in rsvg-convert magick go; do
  command -v "$tool" >/dev/null || { echo "missing $tool – see header of $0" >&2; exit 1; }
done

LOGO=docs/logo.svg
MARK=assets/icon/mark.svg
TRAY=assets/icon/tray-template.svg
OUT=assets/icon
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# png SIZE OUT [SRC] – SRC defaults to mark below 128 px, full logo from 128 px up
png() {
  local size=$1 out=$2 src=${3:-}
  if [[ -z $src ]]; then
    if (( size < 128 )); then src=$MARK; else src=$LOGO; fi
  fi
  mkdir -p "$(dirname "$out")"
  rsvg-convert -w "$size" -h "$size" "$src" -o "$out"
}
# strip timestamps so regenerating doesn't produce noisy diffs
im() { magick "${@:1:$#-1}" -define png:exclude-chunks=date,time,tIME "${@: -1}"; }

echo "macOS .icns"
if command -v iconutil >/dev/null; then
  set_dir=$tmp/pdb-print.iconset
  mkdir -p "$set_dir"
  for s in 16 32 128 256 512; do
    png "$s" "$set_dir/icon_${s}x${s}.png"
    png $((s * 2)) "$set_dir/icon_${s}x${s}@2x.png"
  done
  mkdir -p $OUT/macos
  iconutil -c icns "$set_dir" -o $OUT/macos/pdb-print.icns
else
  echo "  iconutil not found (not macOS?) – skipping .icns" >&2
fi

echo "Windows .ico"
ico_png=()
for s in 16 20 24 32 40 48 64 128 256; do
  png "$s" "$tmp/ico-$s.png"
  ico_png+=("$tmp/ico-$s.png")
done
mkdir -p $OUT/windows
magick "${ico_png[@]}" $OUT/windows/pdb-print.ico

echo "Linux hicolor"
for s in 16 22 24 32 48 64 128 256 512; do
  png "$s" "$tmp/h.png"
  mkdir -p "$OUT/linux/hicolor/${s}x${s}/apps"
  im "$tmp/h.png" -strip "$OUT/linux/hicolor/${s}x${s}/apps/pdb-print.png"
done
mkdir -p $OUT/linux/hicolor/scalable/apps
# drop the embedded provenance metadata from the shipped copy
perl -0pe 's#<metadata>.*?</metadata>##s' $LOGO > $OUT/linux/hicolor/scalable/apps/pdb-print.svg
grep -q '<svg' $OUT/linux/hicolor/scalable/apps/pdb-print.svg || { echo "scalable SVG came out empty" >&2; exit 1; }

echo "Tray (template)"
mkdir -p $OUT/tray
for s in 16 32; do
  png "$s" "$tmp/t.png" $TRAY
  im "$tmp/t.png" -strip "$OUT/tray/tray-$s.png"
  png $((s * 2)) "$tmp/t.png" $TRAY
  im "$tmp/t.png" -strip "$OUT/tray/tray-$s@2x.png"
done
for s in 22 24; do
  png "$s" "$tmp/t.png" $TRAY
  im "$tmp/t.png" -strip "$OUT/tray/tray-$s.png"
done

echo "Web favicon"
mkdir -p $OUT/web
cp $MARK $OUT/web/favicon.svg
png 16 "$tmp/f16.png"; png 32 "$tmp/f32.png"; png 48 "$tmp/f48.png"
magick "$tmp/f16.png" "$tmp/f32.png" "$tmp/f48.png" $OUT/web/favicon.ico
# iOS fills transparency with black, so give the touch icon a solid background
png 160 "$tmp/touch.png"
im -size 180x180 xc:'#e9fbfd' "$tmp/touch.png" -gravity center -composite -strip $OUT/web/apple-touch-icon.png

echo "GitHub social preview"
rsvg-convert -w 1280 -h 640 $OUT/social-preview.svg -o "$tmp/social-bg.png"
png 520 "$tmp/social-logo.png" $LOGO
im "$tmp/social-bg.png" "$tmp/social-logo.png" -geometry +70+60 -composite -strip $OUT/social-preview.png

echo "Windows resource (go-winres)"
cp $OUT/windows/pdb-print.ico cmd/pdb-print/winres/icon.ico
# Release builds stamp the version: add --product-version=git-tag --file-version=git-tag
go run $GO_WINRES make --arch amd64,arm64 --in cmd/pdb-print/winres/winres.json --out cmd/pdb-print/rsrc

echo "done"
