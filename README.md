# shared-memory-mcp

Cloud-backed, project-scoped memory for AI coding agents, with a local SQLite cache so reads are instant and writes survive network drops. Install the same binary on every device you use; agents in Claude Code, Cursor, Windsurf, Copilot, Devin — anything that speaks MCP — share the same knowledge graph per project.

Backend: a Postgres database in your own Supabase project. Storage is a typed knowledge graph (entities → observations + relations) with SQLite FTS5 locally and Postgres tsvector remotely.

## Architecture in one paragraph

The MCP server holds the data **locally** in a SQLite cache and the **truth** in your Supabase Postgres. Reads always hit SQLite (~1 ms). Writes hit SQLite, are forwarded to Postgres synchronously (~20–50 ms when online), and are **queued** to a `pending_writes` table if Postgres is unreachable. A background goroutine drains the queue when connectivity returns and pulls deltas from Postgres every minute (configurable). Postgres assigns the authoritative `updated_at` on every row, so device clock skew can never reorder writes globally.

## How it works

- **One MCP server instance per agent session**, scoped to one project. The project ID is resolved at startup and "pinned" — every query is implicitly filtered to it.
- **Project identity follows the project, not the device.** A `.shared-memory.json` marker file in your repo root, or your git remote, decides which memory you connect to. Clone the repo on a second machine, run the same binary — same memory, zero config.
- **Memory policy lives in the memory itself.** A special `__policy` entity holds the rules. The server reads them at startup and surfaces them to the agent via the MCP `initialize.instructions` field — the model sees the policy without needing a tool call. A `__defaults` template is seeded so fresh projects get a sensible policy out of the box.
- **Single binary, no runtime.** Built in Go with `modernc.org/sqlite` (pure Go SQLite); no Node, no cgo. `go build` and `scp` and you're done.

## Prerequisites

- **Go 1.22+** to build from source. (Pre-built binaries can be released later.)
- A Supabase project. From the dashboard you need:
  - The connection string under Settings → Database → Connection string.
  - **Use the transaction pooler** (port 6543) for the runtime connection string. Setup runs against the direct host (port 5432) where prepared statements work.

## Build

```bash
git clone <this-repo>
cd shared-memory-mcp
go build -o bin/shared-memory-mcp ./cmd/server
go build -o bin/shared-memory-mcp-admin ./cmd/setup
```

Cross-compile from any OS:
```bash
GOOS=linux   GOARCH=amd64 go build -o bin/shared-memory-mcp-linux       ./cmd/server
GOOS=darwin  GOARCH=arm64 go build -o bin/shared-memory-mcp-darwin-arm  ./cmd/server
GOOS=windows GOARCH=amd64 go build -o bin/shared-memory-mcp.exe         ./cmd/server
```

> Note: on Windows we ship the admin tool as `shared-memory-mcp-admin.exe` rather than `…-setup.exe`. Windows UAC heuristics force any binary named `*setup*.exe` to require elevation, which we don't want for a non-privileged CLI.

## One-time setup (per Supabase backend)

Run once, on any machine, with a privileged connection string. Setup writes nothing to disk on the host — it just configures the database and prints the runtime credentials.

```bash
SHARED_MEMORY_SETUP_DB_URL="postgres://postgres:YOUR_PASSWORD@db.PROJECT_REF.supabase.co:5432/postgres" \
  ./bin/shared-memory-mcp-admin init
```

This will:

1. Apply the four migrations in `migrations/` (creates `projects`, `entities`, `observations`, `relations`, `audit_log`, plus `updated_at`/`deleted_at`/`last_writer_device` columns and read-RPCs).
2. Create a dedicated Postgres role `memory_mcp_user` with a random password.
3. Grant it only the privileges it needs (no access to `auth.*` or your other Supabase tables).
4. Seed the `__defaults` project with a `__policy` entity carrying sensible default rules.
5. Print a `config.json` block ready to paste — **this** is what each device gets, not your superuser key.

The printed JSON looks like:

```json
{
  "db": {
    "connectionString": "postgres://memory_mcp_user:abc…@aws-0-us-east-1.pooler.supabase.com:6543/postgres?sslmode=verify-full",
    "caCertPath": null
  },
  "device": { "id": "<uuid>" },
  "project": { "slug": null, "name": null },
  "sync":    { "intervalSeconds": 60, "pageSize": 1000, "localDbPath": null }
}
```

> **Important:** edit the host/port of `connectionString` to your **pooler endpoint** (`aws-0-<region>.pooler.supabase.com:6543`) if setup ran against the direct host. The runtime client requires the transaction pooler.

## Per-device install

For each machine, build the binary, register it with your agent, then **let the server bootstrap its own config on first run**. There's no manual file creation step — when the server starts and its config file is missing, it writes a template at the OS-conventional location with a fresh `device.id`, then exits with a message telling you which two lines to edit.

The OS-conventional locations are:
- Unix: `~/.config/shared-memory-mcp/config.json`
- Windows: `%APPDATA%\shared-memory-mcp\config.json`
- Override the directory with `SHARED_MEMORY_MCP_CONFIG_DIR`.

The flow on a new device:

1. Register the MCP with Claude Code (see next section).
2. Open a Claude Code session — the MCP will fail to connect on first try; check its stderr/logs and you'll see something like:
   ```
   first run: wrote template config to C:\Users\<you>\AppData\Roaming\shared-memory-mcp\config.json.
     1. Run `shared-memory-mcp-admin init` (against your Supabase superuser URL) to get a scoped connection string.
     2. Open ...config.json and replace db.connectionString with the value from step 1.
     3. Restart this MCP session.
   ```
3. Follow those three steps. On Unix also: `chmod 600 ~/.config/shared-memory-mcp/config.json`. On Windows also: `icacls "%APPDATA%\shared-memory-mcp\config.json" /inheritance:r /grant:r "%USERNAME%:F"`.
4. Reload the MCP (`/mcp` in Claude Code or restart the session).

The server refuses to start if the config file is group/world-readable (Unix) or owned by another user (Windows).

## Register with your agent

**Claude Code:**
```bash
claude mcp add shared-memory -- /abs/path/to/bin/shared-memory-mcp
```

**Cursor / Windsurf** (MCP settings JSON):
```json
{
  "mcpServers": {
    "shared-memory": {
      "command": "/abs/path/to/bin/shared-memory-mcp"
    }
  }
}
```

## Project identification — how memory is shared across devices

The server picks the project ID at startup using this precedence:

1. **`.shared-memory.json` in your repo** (recommended). Commit it.
   ```json
   { "slug": "my-team/my-project", "name": "My Project" }
   ```
2. **Git remote URL** (`origin`) of the cwd, auto-detected.
3. **`project.slug`** in `config.json` (per-device fallback).
4. **Cwd basename** (last resort).

The chosen slug + source is logged to stderr at startup.

## Local-first behavior

**What it gives you:**
- Reads: SQLite-fast (~0.1–2 ms typical) regardless of network.
- Read-your-writes: a write you just did is visible to subsequent reads immediately, without waiting for Postgres.
- Network drops are tolerated: writes are queued and replayed when connectivity returns.

**What it doesn't give you:**
- Multi-day offline. The queue lives in `local.db`; if the device dies before drain, those writes are lost.
- True local-only mode is supported (omit `db.connectionString`) — useful for development, but those writes never propagate to other devices until you reconnect.

**Where the cache lives:**
- Unix: `~/.local/share/shared-memory-mcp/local.db`
- Windows: `%LOCALAPPDATA%\shared-memory-mcp\local.db`
- Override via `sync.localDbPath` in config, or `SHARED_MEMORY_LOCAL_DB` env.

You can safely delete `local.db` at any time. The next startup will bootstrap-pull from Postgres.

**Sync interval:** `sync.intervalSeconds` (default 60). Override per-process with `SHARED_MEMORY_SYNC_INTERVAL=10`.

## Memory policy

The server doesn't enforce a policy; the agent decides what to save. But the *rules* the agent follows live in your memory itself, in an entity called `__policy`. The server reads them at startup and passes them to the agent via the MCP `initialize.instructions` field, so the model sees them as system context from turn 1.

A fresh setup seeds a default policy in the `__defaults` project, which the server falls back to if the current project has no `__policy` yet. To customize for a specific project, ask your agent:

> Add an observation to `__policy` saying "always tag decisions with `decision:<short-name>` and link them via `affects` relations to the relevant feature entities".

Or call `add_observations` on `__policy` directly. Changes propagate to every device on the next sync cycle.

## Security model

- **Two-phase credentials.** Your superuser/service-role key is used only during `init`/`migrate`/`rotate-credentials` and is never written to disk on the runtime devices. The runtime credential is a Postgres role scoped only to the memory tables and RPCs.
- **TLS verify-full.** Connections to Supabase use the standard Postgres trust store. For defense in depth, set `db.caCertPath` to your Supabase project's CA bundle.
- **Per-project scope.** The server pins one project ID at startup. Every query filters by it. A compromised config grants access to one project's memory, not your entire Supabase.
- **Per-device audit log.** Every mutation is recorded with `device_id`, tool name, and an args fingerprint. Query `audit_log` to see "what did each device write, and when".
- **Fail-closed file perms.** Server refuses to start if `config.json` is readable by other users.

## Credential rotation

```bash
SHARED_MEMORY_SETUP_DB_URL="postgres://postgres:...@db.PROJECT_REF.supabase.co:5432/postgres" \
  ./bin/shared-memory-mcp-admin rotate-credentials
```

Prints a fresh connection string. Every device with the old one stops working until you update its `config.json`.

## Migrations

Schema lives in `migrations/NNN_*.sql`, embedded into the binary at build time. To apply new migrations after pulling a new version:

```bash
SHARED_MEMORY_SETUP_DB_URL="..." ./bin/shared-memory-mcp-admin migrate
```

Idempotent (uses a `schema_migrations` table).

## Admin CLI subcommands

`shared-memory-mcp-admin` exposes four operations, all needing `SHARED_MEMORY_SETUP_DB_URL`:

- `init` — first-time setup (apply migrations, create scoped role, seed `__defaults/__policy`).
- `migrate` — apply any new migration files added since last run.
- `rotate-credentials` — regenerate the scoped role's password and print the new connection string.
- `reset-policy` — wipe `__defaults/__policy` observations and reseed from the binary's `defaultPolicy` array (use after editing the policy in source).
- `dump [-project <slug>] [-limit N]` — show projects, recent entities and per-device activity from Supabase. Useful for cross-device debugging without opening the SQL editor.

## Troubleshooting

- **`prepared statement does not exist`** — you're connected to the direct host (5432), not the pooler (6543). The runtime client requires the transaction pooler. Fix `connectionString` in `config.json`.
- **`Config file ... is group/world-readable`** — `chmod 600` (Unix) or `icacls` (Windows). See setup steps above.
- **`missing device.id`** — config wasn't loaded or `device.id` is empty. Confirm path: `~/.config/shared-memory-mcp/config.json` on Unix, `%APPDATA%\shared-memory-mcp\config.json` on Windows. Override the dir with `SHARED_MEMORY_MCP_CONFIG_DIR`.
- **`bootstrap pull: …`** at startup — non-fatal; the server starts in "partial cache" mode and the periodic sync catches up.
- **Pending writes piling up** — query `select count(*) from pending_writes` in `local.db`. If it stays high, check the server stderr for the actual push error.

## What this isn't

- Not a semantic-search engine. Full-text only (SQLite FTS5 / Postgres tsvector). Embeddings are a v2 candidate.
- Not multi-user / team-aware. Single-user-multi-device threat model. Adding RLS for team sharing layers on top of the existing role.
- Not a managed service. You host the Supabase project; the MCP just talks to it.
