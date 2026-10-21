# T-knowledge Personal

A store for what you know, kept apart by how well each piece stands. Your AI assistant reads and writes it over MCP; you sign.

- A sentence you are still guessing at is an **assumption** (`A`). Each one says how it could end: the observation that would raise it or refute it.
- A sentence that can be checked is admitted as a **fact** (`T`) only with a basis: a document it rests on, or a derivation from principles and other facts.
- The ledger is append-only. A claim is never edited; when it turns out wrong it is demoted, with a reason, and its history stays.
- The store keeps no files. A document is the text read out of the evidence, how it was looked at, and where the original lives (a link, a path, a hash).

This repository is the Personal edition: one person's store, on their own machine. The hosted Community runs the same core.

## Run it with Docker

You need Docker with Compose.

```sh
curl -O https://raw.githubusercontent.com/ankayma/tknowledge-personal/main/docker-compose.yml   # or clone this repository
TK_OWNER=you@example.com TK_DB_PASSWORD='a long password' docker compose up -d
docker compose logs tk | grep -A1 "private link"
```

On first start the store makes its own signing key in the `tkdata` volume and prints a private link:

```
http://localhost:8080/mcp/t/<grant>
```

The link is the key to your store; treat it like a password. It is also kept in the volume at `/data/owner.grant`.

### Connect your assistant

- **Claude Code:** `claude mcp add --transport http tk 'http://localhost:8080/mcp/t/<grant>'`
- **Claude Desktop, Cursor and other clients that start local commands:**
  ```json
  {"mcpServers": {"t-knowledge": {"command": "npx", "args": ["-y", "mcp-remote", "http://localhost:8080/mcp/t/<grant>"]}}}
  ```
- **claude.ai and other web clients** call the server from the internet, so `localhost` does not reach them. Put the store behind a domain with HTTPS (a reverse proxy or a tunnel to port 8080), set `TK_PUBLIC_URL` to that address, and connect `https://<your-domain>/mcp/t/<grant>`.

Then ask: *"Read T-knowledge's instructions and tell me what it will record as a fact, what it keeps as a guess, and what it will refuse."*

### Settings

| Variable | Meaning |
|---|---|
| `TK_OWNER` | Your email: the person who signs in this store. Required. |
| `TK_DB_PASSWORD` | Password of the Postgres superuser in the compose file. Required. |
| `TK_PUBLIC_URL` | The address people connect to. Default `http://localhost:8080`. |
| `TK_SITE_URL` | Your own site; the pages link to it and mail signs off with it. Optional. |
| `RESEND_API_KEY`, `MAIL_FROM` | Optional. Turns on sign-in by an emailed code (OAuth) for clients that connect without the private link. `MAIL_FROM` must be on a domain verified with [Resend](https://resend.com). |

Your data lives in two Docker volumes: `pgdata` (the database) and `tkdata` (the signing key and the owner's link). Back up both. `docker compose down` keeps them; `docker compose down -v` deletes them.

## Build from source

Go 1.25 or later. The tests start Postgres and Redis in Docker through testcontainers.

```sh
go build ./...
go test ./...
docker build -t tknowledge/tk-personal:latest .
```

| Path | What it is |
|---|---|
| `cmd/tk-api` | The REST API and the only write path into the ledger; runs the migrations on start. |
| `cmd/tk-mcp` | The MCP server; a client of tk-api. |
| `cmd/tk-proxy` | One origin for both: `/mcp` to tk-mcp, everything else to tk-api. |
| `cmd/tk` | Command line: tokens, import and export, setup. |
| `cmd/tk-checker`, `cmd/tk-cdc` | Optional: a sweep that flags claims, and a change feed of the ledger to Redis. |
| `migrations/` | The schema. The `ledger` table is never updated or deleted from; Postgres roles enforce it. |
| `deploy/personal/` | The container's start script. |

## Security model

- The store signs its own grants with an Ed25519 key made on first start. Nothing in this repository or the image carries a key.
- tk-api reads and writes as a non-superuser role, so row-level security applies to every request.
- The private link carries a grant for one person and one assistant. Revoke one with `tk token revoke`.

## Licence

Elastic License 2.0 — see [LICENSE](LICENSE). The source is open for audit. You may not offer the software to others as a hosted or managed service, nor move, change, disable or circumvent its licence key functionality. The commits published before 21 October 2026 were released under AGPL-3.0.

Copyright © 2026 Bao Trinh.
