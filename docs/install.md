# Install and run agent-inbox

This page shows how to install the `agent-inbox`, `inboxd`, and
`agent-inbox-mcp` programs and run them. It covers every install method, a
local server, MCP clients, and fixes for common first-run problems. For a
guided first run from nothing to a working exchange, follow
[Getting started](getting-started.md) instead.

## Requirements

The commands are written for Linux and macOS, in `bash` or `zsh`. They were
checked on Linux. Windows is untested; use WSL 2 and follow the Linux
commands.

| You need | Version | For |
| --- | --- | --- |
| [Go](https://go.dev/doc/install) | 1.24.0 or newer | Building the three programs. Required for every install method. |
| Git | any | Building from a clone, and the Docker Compose setup. |
| Docker Engine with the Compose plugin | any current release | Only for running the server with Docker Compose. |
| Python 3 | any current release | Only for the notification viewer, `make compose-smoke`, and `make walkthrough`. `make walkthrough` also needs the `cryptography` package (`python3 -m pip install cryptography`). |

Go 1.21 to 1.23 can also build the programs if `GOTOOLCHAIN` is `auto`, the
default for Go from <https://go.dev/dl/>. Go then downloads a newer toolchain
on the first build. Some packaged Go builds and the `golang` container images
set `GOTOOLCHAIN=local`, which stops that download; check with
`go env GOTOOLCHAIN`.

The project publishes no prebuilt binaries and no release tags. The only
published artifact is the server's container image,
`ghcr.io/ryannmicua/agent-inbox`, described in the
[operator guide](operators.md#requirements-and-first-start). It contains
`inboxd` only.

## Install the binaries

The SQLite and PostgreSQL drivers are pure Go, so every method builds
without cgo or a C compiler.

### With `go install`

```sh
go install github.com/ryannmicua/agent-inbox/cmd/...@latest
```

This builds all three programs from the latest commit on the default branch
and puts them in `$(go env GOPATH)/bin`, or in `$(go env GOBIN)` when that is
set. To install one program, name it, for example
`go install github.com/ryannmicua/agent-inbox/cmd/agent-inbox@latest`.

To build a specific commit, replace `@latest` with `@<commit>`, for example
`@3e4e1e9`.

### From a clone

```sh
git clone https://github.com/ryannmicua/agent-inbox.git
cd agent-inbox
make build
```

`make build` writes the three programs to `bin/`, built with cgo off and
`-trimpath`. Without `make`, run `go build -o bin/ ./cmd/...` instead.

### Put the programs on your `PATH`

Your shell finds a program by name only if its directory is on `PATH`. Add
the directory for this terminal:

```sh
# After go install:
export PATH="$(go env GOPATH)/bin:$PATH"
# After a clone build, from inside the clone:
export PATH="$PWD/bin:$PATH"
```

To keep the change, add the same line to `~/.bashrc` (Linux `bash`) or
`~/.zshrc` (macOS `zsh`). For a clone build, write the full path to `bin`
instead of `$PWD/bin`.

### Check the installation

```sh
agent-inbox version
inboxd version
agent-inbox-mcp --version
```

Each prints its name and the version Go recorded at build time, for example
`agent-inbox v0.0.0-20261016050527-3e4e1e939178`. Without release tags, the
version is a Go pseudo-version: the commit time and the first 12 characters
of the commit. A clone build prints the same form, with `+dirty` added if the
clone has uncommitted changes, or `(devel)` when Go found no version control
information.

`agent-inbox help`, `inboxd help`, and `agent-inbox-mcp --help` list each
program's commands and settings.

### Update or uninstall

To update after `go install`, run the same `go install` command again. To
update a clone build, run `git pull` and `make build` in the clone.

To uninstall, delete the three files. After `go install`:

```sh
rm "$(go env GOPATH)/bin/agent-inbox" "$(go env GOPATH)/bin/inboxd" "$(go env GOPATH)/bin/agent-inbox-mcp"
```

For a clone build, delete the clone.

## Run a local server

A server needs a registry file listing the agents it accepts. Steps 4 and 5
of [Getting started](getting-started.md#step-4-generate-a-key-for-each-agent)
create keys and a registry for two agents. The
[operator guide](operators.md#registry-operations) describes the registry
format in full.

### Directly with `inboxd`

`inboxd` takes its settings from environment variables. For a local server
on port 8080:

```sh
INBOX_LISTEN_ADDR=127.0.0.1:8080 \
INBOX_DB_PATH="$PWD/inbox.db" \
INBOX_REGISTRY_PATH="$PWD/registry.json" \
INBOX_WEBHOOK_URL=http://127.0.0.1:8081/notifications \
inboxd
```

Check it from another terminal in the same directory:

```sh
INBOX_LISTEN_ADDR=127.0.0.1:8080 INBOX_DB_PATH="$PWD/inbox.db" inboxd healthcheck
```

It prints `healthy`.

`INBOX_WEBHOOK_URL` is required. The server posts human-visible events there,
such as `message.accepted`. When nothing listens, the server logs
`human notification ... failed` for each event and delivers messages as
normal. To see the events, run the bundled viewer from a clone in a third
terminal:

```sh
WEBHOOK_SINK_HOST=127.0.0.1 python3 scripts/webhook_sink.py
```

It prints one JSON line per event. The
[configuration reference](operators.md#configuration-reference) lists every
setting.

### With Docker Compose

The Compose setup runs `inboxd` in a hardened container, with SQLite in a
Docker volume and the notification viewer as a second container. It is the
setup the [operator guide](operators.md) builds on. Agents still need the
`agent-inbox` CLI on the host.

From a clone, with your registry at `$HOME/agent-inbox-demo/registry.json`:

```sh
cp .env.example .env
INBOX_REGISTRY_FILE="$HOME/agent-inbox-demo/registry.json" docker compose up -d --build
docker compose exec -T server inboxd healthcheck
```

The first run builds the image, which takes a few minutes. The health check
prints `healthy`; if it prints an error, wait ten seconds and run it again.
`docker compose ps` shows `server` and `notification-sink` as `(healthy)`.

The server listens on `http://127.0.0.1:8080`, so agents use
`export INBOX_SERVER=http://127.0.0.1:8080` as in the tutorial. See the
events with `docker compose logs notification-sink`. Stop the server and
delete its data with `docker compose down --volumes`.

The container reads the registry as a different user, so keep the file
readable by everyone: `chmod 644 registry.json`. If it is not, a running
server answers every request with `authentication_failed`, and a starting
server restarts in a loop, logging
`registry is not ready: read registry: open /etc/agent-inbox/registry.json: permission denied`.

## Connect an MCP client

An MCP client starts `agent-inbox-mcp` as a local program and talks to it
over standard input and output. The adapter acts as one registered agent.
The client does not start in your working directory, so give it absolute
paths. Print them:

```sh
command -v agent-inbox-mcp
echo "$HOME/agent-inbox-demo/agent-a.key"
```

For Claude Code, register the adapter with your paths in place of the
examples:

```sh
claude mcp add agent-inbox \
  -e INBOX_SERVER=http://127.0.0.1:8080 \
  -e INBOX_AGENT=agent-a \
  -e INBOX_KEY=/home/you/agent-inbox-demo/agent-a.key \
  -- /home/you/go/bin/agent-inbox-mcp
```

`claude mcp list` should then show `agent-inbox` as `Connected`. The
registration applies to the directory you ran it in; add `-s user` after
`add` to use it in every directory. Remove it later with
`claude mcp remove agent-inbox`.

Other clients, such as Claude Desktop and most editors, read a JSON server
list. Add this entry, with your paths:

```json
{
  "mcpServers": {
    "agent-inbox": {
      "command": "/home/you/go/bin/agent-inbox-mcp",
      "env": {
        "INBOX_SERVER": "http://127.0.0.1:8080",
        "INBOX_AGENT": "agent-a",
        "INBOX_KEY": "/home/you/agent-inbox-demo/agent-a.key"
      }
    }
  }
}
```

Restart the client. It lists six tools: `list_spaces`, `read_note`,
`propose_note_revision`, `send_message`, `poll_inbox`, and `ack_message`.
The [agent guide](agents.md#10-use-the-inbox-from-an-mcp-client) describes
each tool.

If the client reports that the server failed, run the adapter check in
[Getting started step 13](getting-started.md#step-13-use-the-inbox-through-mcp)
with the same paths and settings. The adapter writes errors to standard
error, which most clients keep in their MCP log.

## Troubleshooting

Find the first error text you see, then apply the fix. The
[operator guide](operators.md#troubleshooting-common-rejections) lists every
error code the server returns.

### Installing

| You see | Cause | Fix |
| --- | --- | --- |
| `go: command not found` | Go is not installed, or not on `PATH`. | Install Go from <https://go.dev/doc/install>, then open a new terminal. |
| `requires go >= 1.24.0 (running go 1.22.12; GOTOOLCHAIN=local)` | Your Go is older than 1.24 and may not download a newer toolchain. | Install Go 1.24 or newer, or run the command again with `GOTOOLCHAIN=auto` in front. |
| `dial tcp: lookup proxy.golang.org` or `i/o timeout` | Go cannot download modules. | Check your network. Behind a proxy, set `HTTPS_PROXY`, or ask your network administrator which `GOPROXY` to use. |
| `agent-inbox: command not found` | Go's `bin` directory is not on `PATH` in this terminal. | Run the `export PATH=...` line from [Put the programs on your PATH](#put-the-programs-on-your-path). |
| `unknown command "version"`, `flag provided but not defined: -version`, or `INBOX_WEBHOOK_URL is required` from a `version` command | An old build, from before the `version` command, comes first on `PATH`. | Run `command -v agent-inbox` to find it, then remove or rebuild it. |

### Starting the server

| You see | Cause | Fix |
| --- | --- | --- |
| `inboxd: INBOX_WEBHOOK_URL is required` | The settings did not reach `inboxd`. | Check that each line before `inboxd` ends with `\`, with nothing after it. |
| `create database directory: mkdir /var/lib/agent-inbox: permission denied` | `INBOX_DB_PATH` is not set, so `inboxd` uses its container default. | Set `INBOX_DB_PATH` to a file in a directory you can write. |
| `registry is not ready: read registry: open /etc/agent-inbox/registry.json: no such file or directory` | `INBOX_REGISTRY_PATH` is not set, or names a missing file. | Set it to the full path of your `registry.json`. |
| `registry is not ready: parse registry JSON: ...` | The registry is not valid JSON. | Fix the file, or write it again as in [Getting started step 5](getting-started.md#step-5-register-both-agents). |
| `bind: address already in use` | Another program uses the port. `inboxd` logs `listening on` just before this error. | Stop the other program, or choose another port in `INBOX_LISTEN_ADDR` and `INBOX_SERVER`. |
| `human notification message.accepted failed` | Nothing listens on `INBOX_WEBHOOK_URL`. | Expected locally; delivery is not affected. Run the [notification viewer](#directly-with-inboxd) to see events. |
| Compose `server` shows `Restarting`, and `docker compose logs server` shows `registry is not ready: ... permission denied` | The container cannot read the registry file. | Run `chmod 644` on the registry file, then `docker compose up -d` again. |
| `database file "/var/lib/agent-inbox/inbox.db" is unavailable` from `inboxd healthcheck` | `healthcheck` did not get the server's `INBOX_DB_PATH`. | Run it with the same `INBOX_DB_PATH` and `INBOX_LISTEN_ADDR` as the server. |

### Sending, polling, and acknowledging

| You see | Cause | Fix |
| --- | --- | --- |
| `lookup inbox.example.com ... no such host` | `INBOX_SERVER` is not set in this terminal, so the CLI uses its example default. | Run `export INBOX_SERVER=http://127.0.0.1:8080`, or pass `--server`. |
| `connect: connection refused` | The server is not running, or listens on another port. | Start it, and match the port in `INBOX_SERVER`. |
| `--agent and --key (or INBOX_AGENT and INBOX_KEY) are required` | The command lacks `--agent` or `--key`. | Add both. |
| `load signing key: open agent-a.key: no such file or directory` | The key file is not in the current directory. | Run the command from the directory that holds the key, or pass the key's full path. |
| `authentication_failed, HTTP 401` | The server could not verify the request. It does not say why, on purpose. | Check, in order: `--agent` matches an `id` in the registry; the `--key` file belongs to that agent; the registry holds that agent's current `.pub` content; the computer's clock is within five minutes of the server's; and, under Compose, the registry file is readable by everyone (`chmod 644`). |
| `recipient_not_allowed, HTTP 403` | The recipient is not in the sender's `allowed_recipients`, or is in another tenant. | Fix the registry. The server rereads it on every request, so no restart is needed. |
| `kind_not_allowed` | The sender's `allowed_kinds` lacks that kind. | Add the kind to the registry. |
| `payload must be valid JSON` | The `--payload` value is not JSON, often because the shell removed its quotes. | Put the JSON in single quotes: `--payload '{"task":"say hello"}'`. |
| `message_not_found, HTTP 404` on `ack` | The ID is wrong, or the message was not delivered to this agent. | Copy the `id` from this agent's own poll output. |
| `invalid_reply_to` or `reply_to_required` | A `result` must answer a message its sender received, and repeat that message's task and thread IDs. | Pass the original message's `id` to `--reply-to`, and its `task_id` and `thread_id` to `--task` and `--thread`. |
