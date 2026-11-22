# The usage report collector (cmd/pdb-print-stats, docs/telemetry.md), built by GoReleaser.
# distroless static: non-root user, no shell. Mount a writable volume at /data.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pdb-print-stats /usr/bin/pdb-print-stats
EXPOSE 8080
ENTRYPOINT ["/usr/bin/pdb-print-stats"]
CMD ["--addr", ":8080", "--file", "/data/reports.jsonl"]
