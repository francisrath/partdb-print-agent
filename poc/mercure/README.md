# POC: pushing agent jobs over Mercure

Throwaway proof of concept for step 3 of the upstream proposal
([Part-DB-server#1625](https://github.com/Part-DB/Part-DB-server/issues/1625)). It is **not part of
the product** and won't be merged as-is. No Part-DB (AGPL) code is in this directory. The small Go
tool plays both sides: it mints tokens and publishes jobs as Part-DB would, and it subscribes and
prints as the agent would, using the agent's real Part-DB client and printing service.

Tested 2026-10-27 with Mercure **v1.1.5**, Part-DB **v2.20.4** (`partdborg/part-db`, FrankenPHP
v1.13.4) and Traefik **v3.7.16**, all local in Docker, against a real QL-810W.

## Findings

1. **Mercure 1.x is a breaking change from 0.x, and that affects the proposal.**
   - Subscribers use `?match=` / `?match_urlpattern=` instead of `?topic=`.
   - Tokens are OAuth 2.0 access tokens: `typ: at+jwt` header; `iss`, `aud` and `exp` are required; grants go in `authorization_details`.
   - The hub needs an `issuer` block per trusted issuer, and `aud` must match its resource identifier.
   - Any Part-DB publisher or token minting must target 1.x.
2. **Part-DB's FrankenPHP image ships an outdated `mercure` block, so its hub doesn't work.**
   - The Caddyfile still uses the 0.x directives `transport_url`, `publisher_jwt` and `subscriber_jwt`, but the image bundles Mercure 1.1.5.
   - Without keys, Part-DB starts and the hub logs errors.
   - **Setting `MERCURE_PUBLISHER_JWT_KEY`/`MERCURE_SUBSCRIBER_JWT_KEY` makes the server fail at startup:** "…work only in compatibility mode… move them into an issuer block".
   - Part-DB doesn't document Mercure, so nobody hits this today. PR3 has to replace the block.
3. **A 1.x `mercure` block fixes it.** Replacing only that block in the image's Caddyfile (see below), the container starts healthy.
4. **Private updates stay private, even with `anonymous` on** (as Part-DB's block has it):

   | subscriber to `…/agents/1` | private job | public update |
   |---|---|---|
   | no token | not delivered (with `anonymous` off: 401) | delivered |
   | token for `…/agents/2` | **not delivered** | delivered |
   | token for `…/agents/1` | **delivered** | delivered |
   | token signed with a wrong key | 401 `invalid_token` | — |

5. **End to end:** a job `{"type":"print","profile":2,"ids":"32"}` published to the hub reached the agent in **4.6 ms**. The agent fetched the label from a real Part-DB and printed it on a QL-810W, confirmed via SNMP **5.6 s** after publishing. Nearly all of that time is rendering and printing.
6. **Behind Traefik with compression on**, events arrived one at a time in **~7 ms**, with no buffering.

### Mercure 1.x block for the FrankenPHP Caddyfile (what PR3 would ship)

```caddyfile
	mercure {
		transport bolt {
			path /data/mercure.db
		}
		issuer {$MERCURE_ISSUER:https://partdb.example.org} {
			publisher {
				jwt {env.MERCURE_PUBLISHER_JWT_KEY}
			}
			subscriber {
				jwt {env.MERCURE_SUBSCRIBER_JWT_KEY}
			}
		}
		resource_identifier {$MERCURE_RESOURCE_IDENTIFIER}
		anonymous
		subscriptions
		{$MERCURE_EXTRA_DIRECTIVES}
	}
```

## Run it

```sh
cd poc/mercure
docker compose up -d                  # Mercure hub on 127.0.0.1:3901, Traefik in front on :3903
go build -o /tmp/mercure-poc .        # or anywhere outside the repo
P=/tmp/mercure-poc; T=https://partdb.example.org/agents
A1=$($P token -key poc-subscriber-secret-0123456789abcdef -topic $T/1)
PUB=$($P token -key poc-publisher-secret-0123456789abcdef -action publish -topic "$T/*")
$P subscribe -topic $T/1 -token "$A1" &            # or: agent -config ... -printer ql810 -- -topic ... -token ...
$P publish -token "$PUB" -topic $T/1 -data '{"type":"print","profile":2,"ids":"32"}'
docker compose down
```

The keys in `compose.yaml` are throwaway POC values.
