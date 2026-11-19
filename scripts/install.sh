#!/bin/sh
# Installs pdb-print on Debian, Ubuntu or Raspberry Pi OS from its GitHub release.
#
#   curl -fsSL https://github.com/francisrath/partdb-print-agent/releases/latest/download/install.sh | sh
#
# It downloads the .deb package for this computer and the release's checksums.txt, checks the
# package against it, installs it with apt-get and shows the setup code. That's all.
set -eu
base=${PDB_PRINT_BASE_URL:-https://github.com/francisrath/partdb-print-agent/releases/latest/download}

# 1. Which package fits this system (dpkg knows; the kernel can be 64-bit on a 32-bit system)
arch=$(dpkg --print-architecture)
case $arch in
  amd64 | arm64 | armhf) ;;
  *) echo "Sorry, pdb-print has no package for $arch yet." >&2; exit 1 ;;
esac

# 2. Download the release's checksums and the package into a temporary folder
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
chmod 755 "$tmp" # apt-get reads the package as its own user
echo "Downloading checksums.txt from $base"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
deb=$(awk -v end="_$arch.deb" 'substr($2, length($2) - length(end) + 1) == end { print $2 }' "$tmp/checksums.txt")
if [ -z "$deb" ]; then echo "This release has no $arch package." >&2; exit 1; fi
echo "Downloading $deb"
curl -fsSL -o "$tmp/$deb" "$base/$deb"

# 3. Check the package is exactly the one the release lists
echo "Checking its SHA-256 checksum"
(cd "$tmp" && awk -v deb="$deb" '$2 == deb' checksums.txt | sha256sum -c -)

# 4. Install it
echo
echo "Installing the package needs administrator rights (sudo): it adds /usr/bin/pdb-print,"
echo "a 'pdb-print' service user allowed to use USB printers, and a background service."
echo "Nothing else is changed."
sudo apt-get install -y "$tmp/$deb"

# 5. Open the setup wizard for 15 minutes and show where (the service may need a moment to start)
echo
for try in 1 2 3 4 5; do
  if pdb-print pair; then exit 0; fi
  echo "Waiting for the pdb-print service to start ($try)…"
  sleep 2
done
echo "The service didn't start: see 'journalctl -u pdb-print'." >&2
exit 1
