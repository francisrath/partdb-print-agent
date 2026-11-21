# The usage report collector (cmd/pdb-print-stats, docs/telemetry.md), built by GoReleaser.
# distroless static: non-root user, no shell. Mount a writable volume at /data.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pdb-print-stats /usr/bin/pdb-print-stats
EXPOSE 8080
ENTRYPOINT ["/usr/bin/pdb-print-stats"]
CMD ["--addr", ":8080", "--file", "/data/reports.jsonl"]
