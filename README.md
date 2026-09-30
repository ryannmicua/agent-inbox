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
doorbell, and exposes the durable inbox through polling. Artifacts are
references only; payloads are small and scanned for common secret patterns.

## Scope

The envelope uses A2A-style message, task, and artifact vocabulary. The pilot
provides a JSON API and server-sent event doorbells; A2A endpoints and a web UI
are not included. Human-authored requests use a separate organization-owned
issue-tracker form; that human front door is outside this pilot. A doorbell is
a prompt to poll, never proof of delivery.

## Start here

- [Operator guide](docs/operators.md): Compose install, registry management,
  configuration, backup and restore, upgrades, monitoring, audit, and
  troubleshooting.
- [Agent quickstart](docs/agents.md): key generation, signed send, poll, ack,
  and doorbell commands.

The default registry is empty. Add and review agents before exposing the
service. For local validation, run `make test` and `make compose-smoke`.

## Build

```sh
go build -o inboxd ./cmd/inboxd
go build -o agent-inbox ./cmd/agent-inbox
```

The SQLite and PostgreSQL drivers are pure Go, so these binaries build without
cgo. See the operator guide before deploying the service behind a
TLS-terminating reverse proxy. `make compose-smoke` exercises SQLite; run
`scripts/compose-smoke.sh postgres` to test PostgreSQL Compose and its shared
behavioral test suite.
