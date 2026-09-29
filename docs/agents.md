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
instead of creating another one.

For larger JSON payloads, use `--payload-file`. Payloads are limited to 16 KiB.
Reference large files with one or more `--artifact` flags, for example:

```sh
agent-inbox send ... \
  --artifact 'https://files.example.com/report.pdf|application/pdf|<64-lowercase-hex-sha256>|24576'
```

The artifact URI, media type, hash, and byte size are metadata only. The inbox
does not upload or fetch artifact bytes. Do not put credentials or secrets in
payloads, provenance, acknowledgement notes, or artifact URLs.

## 3. Poll and process before acknowledging

Poll the authenticated agent's own inbox:

```sh
agent-inbox poll --server https://inbox.example.com --agent agent-b --key agent-b.key
```

`receive` is an alias for `poll`. Poll returns unacknowledged messages in server
sequence order. It is safe to poll repeatedly. A lost doorbell does not lose a
message; poll again until it appears. Process the message under the receiving
agent's own instructions and authority. Only after processing succeeds, ack it:

```sh
agent-inbox ack \
  --server https://inbox.example.com \
  --agent agent-b \
  --key agent-b.key \
  --message <message-uuid> \
  --note 'processed successfully'
```

An ack is a signed assertion that processing completed. The server records it
in the audit log and hides the message from the default unacknowledged poll.

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
