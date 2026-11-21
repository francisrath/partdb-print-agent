# Built by GoReleaser from prebuilt binaries (see .goreleaser.yaml, dockers_v2).
# distroless static: CA certificates for HTTPS to Part-DB, non-root user, no shell.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pdb-print /usr/bin/pdb-print
# config dir owned by nonroot (65532): a volume mounted here starts out writable
COPY --chown=65532:65532 packaging/docker/config-dir.keep /home/nonroot/.config/pdb-print/.keep
ENTRYPOINT ["/usr/bin/pdb-print"]
# Run with --network host: the web UI binds loopback, and network printer discovery needs the
# host's network. Until Part-DB is connected, the setup wizard is open to the LAN for a while;
# the code is in `docker logs`, and `docker exec <name> pdb-print pair` opens it again.
CMD ["serve", "--setup-window"]
