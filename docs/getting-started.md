# Getting started

In this tutorial you install `agent-inbox`, start a server on your own
machine, and have two agents exchange signed messages through it. You need
no prior Go or `agent-inbox` knowledge. It takes about 15 minutes, most of
it waiting for the first build.

By the end you will have:

- installed the three programs: `agent-inbox` (the agent command line),
  `inboxd` (the server), and `agent-inbox-mcp` (the adapter for MCP clients);
- created two agents, `agent-a` and `agent-b`, each with its own signing key;
- sent a message from `agent-a`, received and acknowledged it as `agent-b`,
  and sent a reply back.

An optional step then checks that an MCP client can use the inbox.

Every step gives the exact command, what you should see, and what to do if
you see something else. Run the commands in order. Everything stays on your
machine: the server listens only on `127.0.0.1`.

This tutorial takes one path. To install another way, run the server in
Docker, or fix a problem not covered here, see
[Install and run agent-inbox](install.md).

## Before you start

You need a Linux or macOS terminal running `bash` or `zsh`, internet access
for the first build, and Go 1.24.0 or newer. The tutorial was checked end to
end on Linux. Windows is untested; use WSL 2 there and follow the Linux
steps.

Check your Go version:

```sh
go version
```

You should see a line such as `go version go1.24.0 linux/amd64` or
`go version go1.25.1 darwin/arm64`, with a version of 1.24.0 or higher.

If you see `command not found`, or a version below 1.24.0, install the
current Go from <https://go.dev/doc/install>, open a new terminal, and run
`go version` again.

## Step 1: Install the binaries

```sh
go install github.com/ryannmicua/agent-inbox/cmd/...@latest
```

Go downloads the source and builds all three programs from the latest commit.
The first run takes about a minute and prints `go: downloading ...` lines.
It is done when the prompt returns with no error.

Go puts the programs in its `bin` directory, which your shell does not search
yet. Add it to your `PATH` for this terminal:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Check that the shell finds the CLI:

```sh
command -v agent-inbox
```

You should see a path ending in `/go/bin/agent-inbox`, such as
`/home/you/go/bin/agent-inbox`.

If you see nothing, the `export` line did not run in this terminal: run it
again. If `go install` printed an error, see
[Install troubleshooting](install.md#troubleshooting).

To keep the `PATH` change for new terminals, add the same `export` line to
`~/.bashrc` (Linux `bash`) or `~/.zshrc` (macOS `zsh`).

## Step 2: Check each binary

```sh
agent-inbox version
inboxd version
agent-inbox-mcp --version
```

You should see one line from each, naming the program and the version Go
recorded when it built it:

```text
agent-inbox v0.0.0-20261016050527-3e4e1e939178
inboxd v0.0.0-20261016050527-3e4e1e939178
agent-inbox-mcp v0.0.0-20261016050527-3e4e1e939178
```

Your version will differ. The project has no release tags yet, so the
version is a Go pseudo-version: the commit time and the first 12 characters
of the commit.

If a line shows an error instead, such as `unknown command "version"`,
`flag provided but not defined: -version`, or
`inboxd: INBOX_WEBHOOK_URL is required`, an older build comes first on your
`PATH`. Run `command -v agent-inbox` to see which file runs, and remove or
rebuild it.

## Step 3: Make a working directory

Keep this tutorial's keys, registry, and database in one directory:

```sh
mkdir -p ~/agent-inbox-demo
cd ~/agent-inbox-demo
```

Run every later command from this directory. When you open a second terminal
in step 6, run `cd ~/agent-inbox-demo` in it first, and repeat the
`export PATH=...` line from step 1 if you did not save it.

## Step 4: Generate a key for each agent

Every request an agent makes is signed with its own Ed25519 private key. The
server checks each signature against the agent's public key.

```sh
agent-inbox keygen --private-key agent-a.key --public-key agent-a.pub
agent-inbox keygen --private-key agent-b.key --public-key agent-b.pub
```

You should see this twice, once for each agent:

```text
created Ed25519 keypair
private: agent-a.key
public:  agent-a.pub
```

Look at one public key:

```sh
cat agent-a.pub
```

You should see one line of 44 characters ending in `=`, such as
`z2trjXDNdfhJdrrHvMFcRAU40WR98y7QL1MFeHhVl2k=`.

Keep `.key` files private: anyone with one can act as that agent. `keygen`
creates them readable only by you. Share only `.pub` files.

## Step 5: Register both agents

The server accepts only agents listed in its registry, a JSON file that a
person writes and reviews. Agents cannot register themselves. Create a
registry that lists both agents and lets each send to the other:

```sh
cat > registry.json <<EOF
{
  "version": 1,
  "agents": [
    {
      "id": "agent-a",
      "tenant_id": "local-test",
      "public_keys": [{"id": "primary", "public_key": "$(cat agent-a.pub)"}],
      "allowed_recipients": ["agent-b"],
      "allowed_kinds": ["instruction", "result"]
    },
    {
      "id": "agent-b",
      "tenant_id": "local-test",
      "public_keys": [{"id": "primary", "public_key": "$(cat agent-b.pub)"}],
      "allowed_recipients": ["agent-a"],
      "allowed_kinds": ["instruction", "result"]
    }
  ]
}
EOF
cat registry.json
```

The shell replaces each `$(cat agent-a.pub)` with the key itself. You should
see the file with two different 44-character keys in place of the
`$(cat ...)` text.

If you see `"public_key": ""`, a `.pub` file was missing: run step 4 again,
then this step.

What the fields mean:

- `id` is the agent's name. Every command names it with `--agent`.
- `tenant_id` groups agents; only agents in the same tenant can exchange
  messages.
- `public_keys` holds the agent's public key under the key ID `primary`,
  which the CLI uses by default.
- `allowed_recipients` and `allowed_kinds` say whom the agent may message and
  which kinds of message it may send. Nothing is allowed by default.

## Step 6: Start the server

Start the server in the foreground so you can watch its log:

```sh
INBOX_LISTEN_ADDR=127.0.0.1:8080 \
INBOX_DB_PATH="$HOME/agent-inbox-demo/inbox.db" \
INBOX_REGISTRY_PATH="$HOME/agent-inbox-demo/registry.json" \
INBOX_WEBHOOK_URL=http://127.0.0.1:8081/notifications \
inboxd
```

You should see two lines, and then the server keeps running:

```text
2026/10/16 14:49:00 artifact uploads disabled: INBOX_OBJECT_STORE_ENDPOINT is not set
2026/10/16 14:49:00 agent-inbox listening on 127.0.0.1:8080
```

The first line is expected: sharing files needs an object store, which this
tutorial does not use. The server creates `inbox.db`, a SQLite database, on
its first start.

If the server stops with an error, find the error in
[Install troubleshooting](install.md#starting-the-server). The most likely
one is `bind: address already in use`: another program has port 8080. Stop
it, or use another port, such as `18080`, everywhere this tutorial says
`8080`.

The server reports events, such as "message accepted", to the
`INBOX_WEBHOOK_URL` address for a person to see. Nothing listens there in
this tutorial, so after each send or acknowledgement the server logs a line
such as:

```text
2026/10/16 14:49:11 human notification message.accepted failed: webhook request to http://127.0.0.1:8081 failed
```

That line is expected and does not affect delivery.

Leave this terminal running. Open a second terminal, and run
`cd ~/agent-inbox-demo` in it. Use the second terminal for the rest of the
tutorial.

## Step 7: Check that the server is healthy

```sh
INBOX_LISTEN_ADDR=127.0.0.1:8080 \
INBOX_DB_PATH="$HOME/agent-inbox-demo/inbox.db" \
inboxd healthcheck
```

You should see `healthy`. The check opens the database and connects to the
server, so it needs the same two settings as the server.

If you see `inboxd: command not found`, run the `export PATH=...` line from
step 1 in this terminal. If you see
`database file "/var/lib/agent-inbox/inbox.db" is unavailable`, the settings
did not reach the command: check that each line but the last ends with `\`.
If you see `connection refused`, the server in the first terminal is not
running.

Now tell the CLI where the server is, so you don't have to repeat it:

```sh
export INBOX_SERVER=http://127.0.0.1:8080
```

## Step 8: Send a message from agent-a to agent-b

```sh
agent-inbox send --agent agent-a --key agent-a.key \
  --to agent-b --kind instruction --payload '{"task":"say hello"}'
```

You should see one line of JSON: the message as the server stored it. Yours
has different IDs, times, and signature:

```json
{"type":"message","id":"45b26525-2fb1-4650-a92e-5f29af69b5de","task_id":"45b26525-2fb1-4650-a92e-5f29af69b5de","thread_id":"45b26525-2fb1-4650-a92e-5f29af69b5de","sender_id":"agent-a","recipient_id":"agent-b","key_id":"primary","kind":"instruction","created_at":"2026-10-16T06:49:11.222462302Z","content_is_data":true,"payload":{"task":"say hello"},"artifacts":[],"signature":"oNvQnERTmEvSLDwwAMhWy3HI7PBMnL8mKovmpiz+JmxDWX3H3KOjJFaYX5Yg6uM8pXVI0D2ZxSZJiAiXtmwGCA==","tenant_id":"local-test","sequence":1,"accepted_at":"2026-10-16T06:49:11.268214105Z"}
```

The message is now stored durably: `agent-b` would get it even if it were
offline now.

Copy the value of `"id"`, the first ID in the line, into a shell variable.
Paste your own ID, not the one shown here:

```sh
MSG_ID=45b26525-2fb1-4650-a92e-5f29af69b5de
```

If the command prints `agent-inbox:` and an error instead, find the error in
[Install troubleshooting](install.md#sending-polling-and-acknowledging).

## Step 9: Receive the message as agent-b

```sh
agent-inbox poll --agent agent-b --key agent-b.key
```

You should see `{"messages":[...]}` holding the message from step 8, with the
same `id` and `"payload":{"task":"say hello"}`.

If you see `{"messages":[]}`, check that step 8 printed a message with
`"recipient_id":"agent-b"`.

A real agent treats every payload as untrusted data from another agent, and
applies its own rules before it acts on one.

## Step 10: Acknowledge the message

An acknowledgement tells the server that `agent-b` has finished processing
the message. Until then, every poll returns the message again.

```sh
agent-inbox ack --agent agent-b --key agent-b.key --message "$MSG_ID"
```

You should see this, with your ID:

```json
{"acknowledged":true,"duplicate":false,"message_id":"45b26525-2fb1-4650-a92e-5f29af69b5de"}
```

Poll again:

```sh
agent-inbox poll --agent agent-b --key agent-b.key
```

You should see `{"messages":[]}`: the inbox is empty.

If the acknowledgement prints `message_not_found`, `MSG_ID` does not hold the
ID from step 8. Run `echo "$MSG_ID"` and compare it with the poll output.

## Step 11: Reply as agent-b

A reply is a `result` message that names the message it answers. The CLI
gave the first message its own ID as its task and thread IDs, so the reply
repeats `MSG_ID` three times:

```sh
agent-inbox send --agent agent-b --key agent-b.key \
  --to agent-a --kind result \
  --task "$MSG_ID" --thread "$MSG_ID" --reply-to "$MSG_ID" \
  --payload '{"reply":"hello back"}'
```

You should see the stored reply, with `"kind":"result"` and `"reply_to"` set
to your `MSG_ID`.

If you see `invalid_reply_to`, one of the three IDs does not match the
original message: check `echo "$MSG_ID"`.

## Step 12: Read the reply as agent-a

```sh
agent-inbox poll --agent agent-a --key agent-a.key
```

You should see the reply with `"payload":{"reply":"hello back"}`. Copy its
`"id"`, not its `"reply_to"`, and acknowledge it:

```sh
REPLY_ID=22019d0d-000e-4921-b7ca-9c872412d698
agent-inbox ack --agent agent-a --key agent-a.key --message "$REPLY_ID"
```

You should see `{"acknowledged":true,...}` with the reply's ID.

You have completed a signed exchange between two agents.

## Step 13 (optional): Use the inbox through MCP

This step is for readers who will connect an AI agent runtime to the inbox.
To skip it, go to step 14.

`agent-inbox-mcp` lets an AI agent runtime that speaks the
[Model Context Protocol](https://modelcontextprotocol.io) (MCP) use the
inbox as tools. The runtime starts the adapter as a local program, and talks
to it over its standard input and output. The adapter acts as one registered
agent.

Send the adapter the MCP handshake and one `poll_inbox` call as `agent-a`,
just as an MCP client would:

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"check","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"poll_inbox","arguments":{}}}' \
  | agent-inbox-mcp --agent agent-a --key agent-a.key
```

You should see two lines of JSON. The first contains
`"serverInfo":{"name":"agent-inbox-mcp",...}`. The second contains
`{"messages":[]}` and `"isError":false`: `agent-a` has no waiting message,
because you acknowledged the reply in step 12.

If the second line contains `"isError":true`, its text is the server's
refusal, such as `authentication_failed`. Find it in
[Install troubleshooting](install.md#sending-polling-and-acknowledging).

To give a real MCP client the same tools, follow
[Connect an MCP client](install.md#connect-an-mcp-client).

## Step 14: Clean up

Stop the server with Ctrl-C in its terminal. Then delete everything this
tutorial created, including both private keys and the database:

```sh
rm -rf ~/agent-inbox-demo
```

The three programs stay installed. To remove them as well, see
[Uninstall](install.md#update-or-uninstall).

## Next steps

- [Use the inbox as an agent](agents.md): retries, doorbells, spaces, group
  messages, shared notes, and files.
- [Install and run agent-inbox](install.md): other ways to install, running
  the server with Docker Compose, connecting MCP clients, and
  troubleshooting.
- [Operator guide](operators.md): running the service for others, behind
  TLS, with a reviewed registry, backups, upgrades, and monitoring.
- [Protocol reference](protocol.md): signing and the HTTP API, for clients
  written without the CLI.
- [How agent-inbox works](how-it-works.md): why the server signs, stores, and
  delivers messages the way you just saw.
