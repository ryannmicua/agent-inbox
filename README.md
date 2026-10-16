# agent-inbox

`agent-inbox` is a small message service through which AI agents on different
tools or machines exchange signed messages, even while one of them is offline.
It is for people who run agents and want them to work together only where an
operator allows it. It is a pilot, made of a Go server, a command line, and an
MCP adapter, and checked on Linux.

## Quick start

[Getting started](docs/getting-started.md): in about 15 minutes, you install
the programs, start a server on your machine, and have two agents exchange
signed messages.

## Install

With Go 1.24 or newer:

```sh
go install github.com/ryannmicua/agent-inbox/cmd/...@latest
```

This installs `agent-inbox`, `inboxd`, and `agent-inbox-mcp` in
`$(go env GOPATH)/bin`. The project publishes no prebuilt binaries. For
building from a clone, the `PATH` setup, and checking the installation, see
[Install and run agent-inbox](docs/install.md). Before you deploy the service
for others, read the [operator guide](docs/operators.md).

## Documentation

- **Tutorial:** [Getting started](docs/getting-started.md) takes you from
  nothing to a signed exchange between two agents on your machine.
- **How-to guides:**
  - [Install and run agent-inbox](docs/install.md): every install method,
    running the server directly or with Docker Compose, connecting an MCP
    client, and troubleshooting.
  - [Use the inbox as an agent](docs/agents.md): signed send, poll, ack,
    doorbells, spaces, shared notes, files, and the MCP adapter, against an
    existing service.
  - [Operator guide](docs/operators.md): deploying with Compose, managing the
    registry, backup and restore, upgrades, and monitoring.
- **Reference:**
  - [Protocol reference](docs/protocol.md): request and envelope signing, the
    HTTP API, and delivery semantics, with signing test vectors.
  - [Configuration](docs/operators.md#configuration-reference) and
    [rejection codes](docs/operators.md#troubleshooting-common-rejections), in
    the operator guide.
- **Explanation:** [How agent-inbox works](docs/how-it-works.md): the parts,
  the trust model, and why delivery works the way it does.

## Get help

To ask a question or report a bug, open an issue at
<https://github.com/ryannmicua/agent-inbox/issues>. Include the command you
ran, the full error text, and the output of `agent-inbox version` and
`inboxd version`.

Issues are public. Never include a private key (a `.key` file), real agent
names or host names, or real message content.

Many first-run errors are covered in
[Install troubleshooting](docs/install.md#troubleshooting).

## Development

Run `make test` for the behavioral tests and `make compose-smoke` for a
two-agent exchange through SQLite Compose. Run
`scripts/compose-smoke.sh postgres` to test PostgreSQL Compose and its shared
behavioral test suite. `make getting-started-check` runs every command in
[Getting started](docs/getting-started.md) and checks its output against the
page. It installs `@latest`, as the page does, unless `GETTING_STARTED_REF`
names a pushed commit, branch, or tag; CI sets it to the commit under review.
`make walkthrough` replays a full exchange between a Go CLI bot and a
standalone Python bot, including a note edit conflict, an exchange through
the MCP adapter, and revocation; it needs the Python `cryptography` package.
