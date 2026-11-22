# The Inno Setup compiler (ISCC) under Wine, to build setup.exe on Linux (release.yml, job
# release-windows). Inno Setup is only a build tool here: nothing of it ends up linked into pdb-print.
#   docker build --platform linux/amd64 -t pdb-print-inno packaging/windows/inno
#   docker run --rm --platform linux/amd64 -v "$PWD/dist/setup:/work" pdb-print-inno /DVersion=1.2.3 pdb-print.iss
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

ENV DEBIAN_FRONTEND=noninteractive WINEDEBUG=-all WINEPREFIX=/opt/wine
# 64-bit Wine only: GitHub's runners don't run 32-bit programs ("socket: Function not implemented")
RUN apt-get update \
 && apt-get install -y --no-install-recommends wine wine64 xvfb xauth ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# The official installer, pinned by version and SHA-256 (from the GitHub release's asset digest)
ARG INNO_VERSION=7.1.1
ARG INNO_TAG=is-7_1_1
ARG INNO_SHA256=8a5a6711c3766a6e5fb74f280ba0e3551e719ce94538d901fdbd1f8f9920b3f0
RUN curl -fsSLo /tmp/is.exe "https://github.com/jrsoftware/issrc/releases/download/${INNO_TAG}/innosetup-${INNO_VERSION}-x64.exe" \
 && echo "${INNO_SHA256}  /tmp/is.exe" | sha256sum -c - \
 && xvfb-run -a wineboot --init && wineserver -w \
 && { xvfb-run -a wine /tmp/is.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP- "/DIR=C:\\inno" "/LOG=C:\\is.log"; rc=$?; wineserver -w; \
      if [ ! -f "$WINEPREFIX/drive_c/inno/ISCC.exe" ]; then echo "Inno Setup install failed (wine exit $rc):"; cat "$WINEPREFIX/drive_c/is.log" 2>/dev/null; exit 1; fi; } \
 && rm -f /tmp/is.exe "$WINEPREFIX/drive_c/is.log"

WORKDIR /work
ENTRYPOINT ["xvfb-run", "-a", "wine", "C:\\inno\\ISCC.exe"]
