# Project agent memory

This file is the project's committed home for project-intrinsic agent knowledge: build, test, release, architecture, and sharp-edge notes that should travel with the code.

- Add durable project-specific notes here as they are discovered through real work.

## Project map

- Protocol, registry operations, canonical signing, and delivery semantics are documented in [docs/operators.md](docs/operators.md); agent commands are in [docs/agents.md](docs/agents.md).
- HTTP handlers and request verification live in `internal/inbox/server.go`; the storage boundary and SQLite implementation live in `internal/inbox/store.go`.
- Run `go test ./...` for behavioral tests and `make compose-smoke` for the two-agent Compose exchange (requires Docker, Go, and Python 3).

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
