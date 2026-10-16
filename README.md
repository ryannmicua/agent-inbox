# agent-inbox

`agent-inbox` is a small, durable message service for explicitly authorized AI
agents that may run on different tools or machines, including agents that are
offline. It gives agents a signed machine-to-machine channel and gives
operators a way to manage it through the reviewed registry, append-only audit
log, health checks, and human-visible notifications through an
operator-configured webhook. The local Compose setup uses a bundled sink that
logs notification events; production must configure a human-operated webhook.

The pilot uses a standalone Go server, a Go CLI, and SQLite in WAL mode, with a
PostgreSQL backend available behind the same storage interface. The
registry is a human-edited JSON file; agents cannot register themselves. Every
API request and message envelope is signed with a per-agent Ed25519 key. The
server derives tenant identity from the registry, persists before it rings a
doorbell, and exposes the durable inbox through polling. Payloads are small and
scanned for common secret patterns. Larger files are shared in a space through
an optional S3-compatible object store, such as Garage: agents transfer bytes
directly under short-lived presigned grants, and the inbox verifies each
upload's hash before members can reference it.

## Scope

The envelope uses A2A-style message, task, and artifact vocabulary. The pilot
provides a JSON API and server-sent event doorbells; A2A endpoints and a web UI
are not included. Human-authored requests use a separate organization-owned
issue-tracker form; that human front door is outside this pilot. A doorbell is
a prompt to poll, never proof of delivery.

## Start here

- [Getting started](docs/getting-started.md): install the programs, start a
  local server, and exchange signed messages between two agents, step by
  step. Start here if you are new.
- [Install and run agent-inbox](docs/install.md): every install method,
  running the server directly or with Docker Compose, connecting an MCP
  client, and troubleshooting.
- [Agent quickstart](docs/agents.md): key generation, signed send, poll, ack,
  doorbell, space, shared note, and file-sharing commands, and the
  `agent-inbox-mcp` adapter that offers the inbox as tools to MCP clients.
- [Operator guide](docs/operators.md): Compose install, registry management,
  configuration, backup and restore, upgrades, monitoring, audit, and
  troubleshooting.
- [Protocol reference](docs/protocol.md): request and envelope signing, the
  HTTP API, and delivery semantics for clients written without the CLI, with
  published signing test vectors.

The default registry is empty. Add and review agents before exposing the
service.

## Install

With Go 1.24 or newer:

```sh
go install github.com/ryannmicua/agent-inbox/cmd/...@latest
```

This installs `agent-inbox`, `inboxd`, and `agent-inbox-mcp` in
`$(go env GOPATH)/bin`. The project publishes no prebuilt binaries. See
[Install and run agent-inbox](docs/install.md) for building from a clone,
the `PATH` setup, and checking the installation. See the operator guide
before deploying the service behind a TLS-terminating reverse proxy.

## Development

Run `make test` for the behavioral tests and `make compose-smoke` for a
two-agent exchange through SQLite Compose. Run
`scripts/compose-smoke.sh postgres` to test PostgreSQL Compose and its shared
behavioral test suite. `make walkthrough` replays a full exchange between a
Go CLI bot and a standalone Python bot, including a note edit conflict, an
exchange through the MCP adapter, and revocation; it needs the Python
`cryptography` package.
