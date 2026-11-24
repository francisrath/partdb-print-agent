# Security

Please report vulnerabilities privately through
[GitHub's private vulnerability reporting](https://github.com/francisrath/pdb-print/security/advisories/new),
not in a public issue. Only the latest release gets fixes.

pdb-print's local API and web UI listen on `127.0.0.1:8631` by default; the design and threat model are in
[docs/architecture.md §10](docs/architecture.md#10-security).
