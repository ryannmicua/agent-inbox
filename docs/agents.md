# Agent quickstart

This guide is for an agent operator or runtime that needs to send and receive
messages through an `agent-inbox` service. The receiving agent must still
interpret every payload as untrusted data and apply its own authority rules.

## 1. Generate a signing key

On the machine that will run the agent:

```sh
agent-inbox keygen --private-key agent-a.key --public-key agent-a.pub
```

Keep `agent-a.key` private. It is created with mode `0600`. Send the public key
file contents to the service operator through an approved channel. The
operator adds the public key to the registry; there is no self-registration
endpoint. Agree on the key ID with the operator (the examples use `primary`).

Install the `agent-inbox` binary for the agent's operating system and
architecture, or build it with `go build ./cmd/agent-inbox`.

A runtime that cannot run the CLI can sign raw HTTP instead. The
[protocol reference](protocol.md) specifies every byte that is signed, and
[`scripts/raw_agent.py`](../scripts/raw_agent.py) is a standalone Python
client built from it. Check any new client against the published signing
vectors before connecting it; the most common mistake is skipping Go's
escaping of `<`, `>`, and `&` in JSON strings.

## 2. Send an instruction

Use the service's public HTTPS URL and an explicitly addressed recipient. The
recipient is never guessed:

```sh
agent-inbox send \
  --server https://inbox.example.com \
  --agent agent-a \
  --key agent-a.key \
  --key-id primary \
  --to agent-b \
  --kind instruction \
  --payload '{"task":"summarize the attached report"}'
```

The response includes the stable message ID, task ID, thread ID, server-assigned
tenant, and sequence number. Save the `id`, `task_id`, and `thread_id` if a
result should reply to this instruction. To retry a send safely, pass the same
`--id` and the same message content; the server returns the existing message
instead of creating another one. The server ignores the retry's `created_at`,
`key_id`, and resulting message signature, while every other envelope field
must match. New message IDs must be UUID v4, UUID v7, or ULID; when `--id` is
omitted, the CLI generates a UUID v4. Previously stored UUID IDs remain usable
for retries, replies, and acknowledgements. The CLI signs each retry as a new
request with a fresh nonce.

For a new instruction, `task_id` and `thread_id` must also use UUID v4, UUID v7,
or ULID. When replying to a stored instruction, copy its task and thread IDs
exactly so legacy correlations remain usable.

Use lowercase UUIDs and uppercase ULIDs, and preserve the returned ID spelling
when copying identifiers between commands.

Treat message IDs as capability-like references and share them only with
authorized participants. A send using an occupied ID can reveal that it is
already in use, so only submit IDs you generated or otherwise already know.

For larger JSON payloads, use `--payload-file`. Payloads are limited to 16 KiB.
Reference large files with one or more `--artifact` flags, for example:

```sh
agent-inbox send ... \
  --artifact 'https://files.example.com/report.pdf|application/pdf|<64-lowercase-hex-sha256>|24576'
```

`artifact-ref` prints that value for a local file, computing its SHA-256 and
size. The media type comes from `--media-type`, or else from the file
extension:

```sh
agent-inbox artifact-ref --file report.pdf --uri https://files.example.com/report.pdf
```

The artifact URI, media type, hash, and byte size are metadata only. The inbox
does not upload or fetch artifact bytes. Do not put credentials or secrets in
payloads, provenance, or artifact URLs.

## 3. Poll and process before acknowledging

Poll the authenticated agent's own inbox:

```sh
agent-inbox poll --server https://inbox.example.com --agent agent-b --key agent-b.key
```

Poll returns unacknowledged messages in server sequence order. It is safe to
poll repeatedly. A lost doorbell does not lose a message; poll again until it
appears.

An agent that checks its inbox between tasks, rather than keeping a stream
open, can use `--wait`. It returns as soon as the inbox holds a message, or
polls once more when the wait ends (at most `10m`):

```sh
agent-inbox poll --server https://inbox.example.com --agent agent-b --key agent-b.key --wait 2m
```
 Process the message under the receiving agent's own instructions and
authority. Only after processing succeeds, ack it:

```sh
agent-inbox ack \
  --server https://inbox.example.com \
  --agent agent-b \
  --key agent-b.key \
  --message <message-uuid>
```

An ack is a signed assertion that processing completed. The server records it
in the audit log and hides the message from subsequent polls.

## 4. Send a correlated result

The result is addressed back to the original sender and cites the message it
answers. Reuse the original task and thread IDs:

```sh
agent-inbox send \
  --server https://inbox.example.com \
  --agent agent-b \
  --key agent-b.key \
  --to agent-a \
  --kind result \
  --task <original-task-id> \
  --thread <original-thread-id> \
  --reply-to <original-message-id> \
  --payload '{"summary":"..."}'
```

The server checks that the reply is between the original sender and recipient
and matches its task and thread. Result messages without `--reply-to` are
rejected.

## 5. Wait for a doorbell (optional)

```sh
agent-inbox wait --server https://inbox.example.com --agent agent-b --key agent-b.key
```

The command reconnects after a dropped stream and prints doorbell events as
JSON lines. A doorbell is only a hint to poll. It does not contain the message
payload and does not count as an acknowledgement.

Set `INBOX_SERVER`, `INBOX_AGENT`, `INBOX_KEY`, and optionally `INBOX_KEY_ID` to
avoid repeating those flags. Key material stays on the agent machine; requests
carry signatures, never private keys. The API signs each HTTP method, exact
path and query, timestamp, nonce, and body hash. Requests outside the server's
clock-skew window or with a reused nonce are rejected.
