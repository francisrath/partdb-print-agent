#!/bin/sh
# Tests install.sh against a fake release and fake system tools (dpkg, curl, sudo, apt-get,
# pdb-print). Nothing is installed. Run: sh scripts/install_test.sh
# shellcheck disable=SC2016,SC2329 # sh -c scripts take $1 on purpose; check calls the helpers
set -eu
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0

# fake release: one package per architecture plus checksums.txt
mkdir -p "$work/release" "$work/bin"
for a in amd64 arm64 armhf; do echo "package $a" > "$work/release/pdb-print_0.3.0_$a.deb"; done
(cd "$work/release" && sha256sum -- *.deb > checksums.txt)

# fake tools: log what they're asked to do
cat > "$work/bin/dpkg" <<'T'
#!/bin/sh
echo "$FAKE_ARCH"
T
cat > "$work/bin/curl" <<'T'
#!/bin/sh
echo "curl $*" >> "$LOG"
while [ $# -gt 1 ]; do [ "$1" = -o ] && out=$2; shift; done
cp "$FAKE_RELEASE/$(basename "$1")" "$out"
T
cat > "$work/bin/sudo" <<'T'
#!/bin/sh
echo "sudo $*" >> "$LOG"
T
cat > "$work/bin/pdb-print" <<'T'
#!/bin/sh
echo "pdb-print $*" >> "$LOG"
T
chmod +x "$work/bin/"*

# run ARCH [RELEASE]: runs install.sh, leaves its output in $out and the tool log in $log
run() {
  log="$work/log"; : > "$log"
  out=$(FAKE_UID=${FAKE_UID:-1000} PATH="$work/bin:$PATH" LOG="$log" FAKE_ARCH=$1 FAKE_RELEASE=${2:-$work/release} \
    PDB_PRINT_BASE_URL=https://example.invalid/download sh "$here/install.sh" 2>&1) && status=0 || status=$?
}
check() { # description, condition…
  d=$1; shift
  if "$@"; then echo "ok   $d"; else echo "FAIL $d"; echo "$out" | sed 's/^/     /'; sed 's/^/     log: /' "$log"; fail=1; fi
}
installs() { [ "$status" -eq 0 ] && grep -q "^sudo apt-get install -y .*/pdb-print_0.3.0_$1.deb$" "$log"; }
paired() { grep -q "^pdb-print pair" "$log"; }

for a in amd64 arm64 armhf; do
  run $a
  check "$a: installs the $a package" installs $a
  check "$a: opens the setup window" paired
done

run armhf # a 64-bit kernel with 32-bit Raspberry Pi OS: dpkg says armhf, uname says aarch64
check "armhf userland: armhf package" installs armhf

run i386
check "unknown arch: fails before downloading" sh -c "[ $status -ne 0 ] && ! grep -q curl '$log'"
check "unknown arch: says why" sh -c 'echo "$1" | grep -q "no package for i386"' _ "$out"

mkdir -p "$work/bad" && cp "$work/release/"* "$work/bad/" && echo tampered >> "$work/bad/pdb-print_0.3.0_arm64.deb"
run arm64 "$work/bad"
check "checksum mismatch: fails before sudo" sh -c "[ $status -ne 0 ] && ! grep -q sudo '$log'"

run arm64
check "explains sudo right before using it" sh -c 'echo "$1" | grep -q "administrator rights (sudo)"' _ "$out"

# as root (no sudo on a minimal Debian): installs without sudo
cat > "$work/bin/id" <<'T'
#!/bin/sh
echo "${FAKE_UID:-1000}"
T
cat > "$work/bin/apt-get" <<'T'
#!/bin/sh
echo "apt-get $*" >> "$LOG"
T
chmod +x "$work/bin/id" "$work/bin/apt-get"
FAKE_UID=0 run amd64
check "root: installs without sudo" sh -c "[ $status -eq 0 ] && grep -q '^apt-get install -y .*_amd64.deb$' '$log' && ! grep -q '^sudo' '$log'"

# not a Debian system: a plain message instead of "dpkg: not found"
mkdir -p "$work/nodpkg" && cp "$work/bin/"* "$work/nodpkg/" && rm "$work/nodpkg/dpkg"
log="$work/log"; : > "$log"
out=$(PATH="$work/nodpkg:/usr/bin:/bin" LOG="$log" FAKE_ARCH=amd64 FAKE_RELEASE="$work/release" \
  PDB_PRINT_BASE_URL=https://example.invalid/download sh "$here/install.sh" 2>&1) && status=0 || status=$?
if command -v dpkg > /dev/null 2>&1; then
  echo "skip no dpkg: this machine has dpkg in /usr/bin"
else
  check "no dpkg: says it needs Debian" sh -c '[ "$2" -ne 0 ] && echo "$1" | grep -q "Debian"' _ "$out" "$status"
fi

exit $fail
