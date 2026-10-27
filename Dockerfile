# Built by GoReleaser from prebuilt binaries (see .goreleaser.yaml, dockers_v2).
# distroless static: CA certificates for HTTPS to Part-DB, non-root user, no shell.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pdb-print /usr/bin/pdb-print
# config dir owned by nonroot (65532): a volume mounted here starts out writable
COPY --chown=65532:65532 packaging/docker/config-dir.keep /home/nonroot/.config/pdb-print/.keep
ENTRYPOINT ["/usr/bin/pdb-print"]
# The web UI only binds loopback: run with --network host to reach it on the host.
CMD ["serve"]
