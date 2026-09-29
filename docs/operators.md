# Operator guide

This guide covers operating the standalone SQLite inbox. The human control
surface is the reviewed registry plus the local audit and monitoring commands;
agents have no registry or administrative endpoint. The service accepts signed
machine requests and sends human-visible event notifications to a required
generic webhook.

## Requirements and first start

- Docker Engine with the Compose plugin.
- A registry file readable by the service. Start from the empty
  [`config/registry.json`](../config/registry.json).
- A reachable human notification webhook URL, configured as
  `INBOX_WEBHOOK_URL` before starting Compose.
- A TLS-terminating reverse proxy for use outside a trusted local test. The
  service itself listens on plain HTTP and does not manage certificates.

1. Clone this repository and enter its directory.
2. Have each agent operator generate an Ed25519 keypair with the CLI. Collect
   only the public key from each agent through a human-approved channel.
3. Edit `config/registry.json` to add the approved agents and their allowed
   recipients and message kinds. See [Registry operations](#registry-operations).
4. Set the webhook endpoint and start the Compose service:

   ```sh
   export INBOX_WEBHOOK_URL="https://hooks.example.com/agent-inbox"
   docker compose up -d --build
   docker compose ps
   ```

5. Confirm the local listener and SQLite health check, then inspect service logs:

   ```sh
   docker compose exec -T server inboxd healthcheck
   docker compose logs --tail=100 server
   ```

Compose binds the host port to `127.0.0.1` and puts the database in the named
`inbox-data` volume. The registry is mounted read-only. A reverse proxy on the
same host can proxy its upstream to `http://127.0.0.1:8080`; a proxy in another
container can join the Compose network and use `http://server:8080`. Terminate
TLS at that proxy and expose only HTTPS to agents. Example external service
URL: `https://inbox.example.com`.

For a local two-agent end-to-end smoke run, execute `make compose-smoke`. It
creates temporary keys and a temporary registry, starts an isolated Compose
project, performs an instruction/result exchange through the CLI, then removes
its test volume.

## Configuration reference

| Setting | Default | Purpose |
| --- | --- | --- |
| `INBOX_LISTEN_ADDR` | `:8080` | HTTP listener inside the container. |
| `INBOX_STORAGE` | `sqlite` | Storage backend. Only `sqlite` ships in this pilot. |
| `INBOX_DB_PATH` | `/var/lib/agent-inbox/inbox.db` | SQLite database path. |
| `INBOX_REGISTRY_PATH` | `/etc/agent-inbox/registry.json` | Human-managed JSON registry path. |
| `INBOX_WEBHOOK_URL` | required | Generic HTTP webhook for consequential human-visible events. Compose refuses to start without it. |
| `INBOX_RETRY_INTERVAL` | `30s` | Delay between doorbell attempts and before unacknowledged escalation. Accepts Go duration syntax. |
| `INBOX_MAX_DOORBELL_ATTEMPTS` | `3` | Total doorbell notifications, including the initial ring, before one escalation. |
| `INBOX_REQUEST_SKEW` | `5m` | Maximum difference between a signed request timestamp and server time. Accepts Go duration syntax. |
| `INBOX_HOST_PORT` | `8080` | Host loopback port published by Compose. |
| `INBOX_REGISTRY_FILE` | `./config/registry.json` | Host path mounted read-only as the registry. |

Each webhook event includes its event name, timestamp, message ID, agent IDs,
tenant, kind, or rejection code as applicable. It never includes message
payloads or key material. Events include `message.accepted`,
`message.acknowledged`, `message.rejected`, and
`message.unacknowledged_escalation`. Webhook failure is logged and does not
undo an already persisted message or ack. Escalation is retried by the
notification loop until the webhook accepts it.

## Registry operations

The registry is JSON so the service can validate and reload a small, strictly
typed file. It is read for each authenticated lookup, so a valid atomic file
replacement takes effect without restarting the server. Keep its source under
human review and make a temporary file before replacing it; malformed JSON
fails closed.

Each agent entry contains:

```json
{
  "id": "agent-a",
  "tenant_id": "tenant-example",
  "public_keys": [
    {"id": "primary", "public_key": "<base64 Ed25519 public key>"}
  ],
  "allowed_recipients": ["agent-b"],
  "allowed_kinds": ["instruction", "result"]
}
```

The top-level object has `"version": 1` and an `"agents"` array. Use unique
agent IDs and key IDs. `public_key` is the base64-encoded 32-byte public key
printed by `agent-inbox keygen`. The envelope vocabulary recognizes
`instruction`, `result`, `finding`, `status`, and `ack`, but this pilot accepts
message sends of `instruction` and `result` only; acknowledgement uses its
separate API. Each sender must still be explicitly allowed each pilot kind and
recipient. There is no `*` recipient and no caller-supplied tenant: the server
assigns the sender's registered tenant and rejects cross-tenant delivery.

### Add an agent

1. Ask the agent operator to generate a keypair and provide the public key and
   desired agent ID through a human-approved process.
2. Review the proposed tenant, key ID, allowed recipients, and kinds.
3. Add the entry and atomically replace the registry file. Keep the private
   key with its agent; never copy it into this repository or server.
4. Confirm the agent can make a signed poll and can only address approved
   recipients.

### Rotate a key

The service supports multiple active key IDs per agent to allow overlap:

1. Have the agent generate a new keypair with distinct file paths.
2. Add the new public key as another `public_keys` entry, such as
   `{"id":"2026-rotation","public_key":"<new base64 key>"}`, while leaving
   the old entry active.
3. Replace the agent's private key and set `--key-id 2026-rotation` (or
   `INBOX_KEY_ID`) after coordinating the change.
4. Verify signed requests using the new key, then remove the old public-key
   entry from the registry. Requests signed by the old key are rejected as
   soon as the registry reloads.

### Revoke an agent or key

For a whole agent, either remove its entry or set `"disabled": true`. For a
single key, set `"disabled": true` on that `public_keys` entry or remove it.
Replace the file and confirm the old credentials receive `unregistered_agent`
or `unknown_key`. Revocation affects new requests immediately; it does not
delete already accepted messages or audit history.

Changing an agent's tenant immediately isolates it from messages stored under
its previous tenant. Those messages remain in the audit and storage history but
no longer appear in that agent's polls, can no longer be acknowledged by it, and
will not trigger further doorbells or escalations to it.

## Signing and delivery behavior

Every HTTP request, including health checks and unknown routes, requires
Ed25519 request headers. Their canonical signing bytes are the UTF-8 text:

```text
agent-inbox-request-v1\n<agent-id>\n<key-id>\n<METHOD>\n<exact-path-and-query>\n<unix-seconds>\n<nonce>\n<lowercase-hex-sha256-of-body>
```

The signed nonce is stored with a uniqueness constraint and the timestamp must
be within the configured skew. Each message has a second signature. Message
signing bytes are `agent-inbox-envelope-v1\n` followed by compact JSON of the sender-authored
fields, in the order `type`, `id`, `task_id`, `thread_id`, optional `reply_to`,
`sender_id`, `recipient_id`, `key_id`, `kind`, `created_at`, optional
`asserted_authority`, `content_is_data`, `payload`, `artifacts`, and optional
`provenance`. The `signature`, server tenant, sequence, acceptance time, and ack
fields are excluded. Object keys in payload and provenance are sorted by
Go's `encoding/json`; array order and JSON number representation are
significant. Payloads and artifacts are signed as data.

The server commits a new message and its accepted-send audit record before it
publishes a doorbell. The SSE stream is in-memory and can lose events; poll is
the delivery method. The receiver should ack only after processing. Repeated
requests with the same message ID and same canonical content return the
original sequence; reusing that ID for different content is a conflict. A
result must cite the original message and match its sender, recipient, task,
and thread. A transport failure means delivery is unknown, not that an agent is
dead.

The signed JSON API is:

| Method and path | Behavior |
| --- | --- |
| `POST /v1/messages` | Submit a signed envelope to its explicit `recipient_id`. |
| `GET /v1/messages?after_seq=N&limit=N` | Poll only the authenticated agent's unacknowledged inbox in sequence order. |
| `POST /v1/messages/{id}/ack` | Acknowledge a message after processing with `{"processed":true,"processed_at":"<RFC3339>"}`. |
| `GET /v1/events` | Open an authenticated SSE doorbell stream for the current agent. |
| `GET /healthz` | Signed storage health check; returns no message data. |

Signed API requests carry `X-Agent-ID`, `X-Key-ID`, `X-Request-Timestamp`,
`X-Request-Nonce`, and `X-Request-Signature` headers. The signature covers the
exact path and query, so reverse proxies must preserve them.

The sender's `asserted_authority` is stored as an assertion only. A receiver
must derive its own authority from its own operating context and must never
inherit or expand authority from message content. `content_is_data` must be
true; it marks how content must be treated but does not make untrusted content
safe to execute.

## Backup and restore

The server uses SQLite WAL mode. Do not copy the live `.db` file alone: recent
committed transactions may still be in the WAL file. Use the online backup
command, which runs SQLite `VACUUM INTO` to create a consistent snapshot while
the service remains up:

```sh
docker compose exec -T server inboxd backup \
  --db /var/lib/agent-inbox/inbox.db \
  --out /tmp/agent-inbox-backup.db
container_id="$(docker compose ps -q server)"
docker cp "$container_id:/tmp/agent-inbox-backup.db" ./agent-inbox-backup.db
```

Store backups encrypted and access-controlled. The database may contain
operator-supplied content, tenant identifiers, and message metadata. Keep
enough dated copies to meet the deployment's recovery objective and verify
restores periodically.

To restore a snapshot, stop the service and use a one-off container under the
service's non-root UID to replace the file in the named volume:

```sh
docker compose stop server
backup="$(pwd -P)/agent-inbox-backup.db"
docker compose run --rm --no-deps \
  -v "$backup:/restore.db:ro" \
  --entrypoint sh server -c '
    rm -f /var/lib/agent-inbox/inbox.db-wal /var/lib/agent-inbox/inbox.db-shm
    cp /restore.db /var/lib/agent-inbox/inbox.db.restore
    mv /var/lib/agent-inbox/inbox.db.restore /var/lib/agent-inbox/inbox.db
    chmod 0600 /var/lib/agent-inbox/inbox.db
  '
docker compose up -d server
docker compose exec -T server inboxd healthcheck
```

The restore container mounts the same Compose named volume. Keep the source
backup outside the database volume and never restore over a running server.

## Upgrades

1. Make and verify an online backup.
2. Review the target release and its schema notes.
3. Build and recreate the service:

   ```sh
   docker compose build --pull
   docker compose up -d --remove-orphans
   docker compose ps
   docker compose exec -T server inboxd healthcheck
   ```

The Compose named volume survives container replacement. This pilot creates
tables idempotently and has no destructive schema migration. Keep the prior
image available until health and audit checks pass. Rollback means stopping the
service and running the prior image against the same compatible database
schema; restore a backup only if the new version changed persistent data.

## Audit, health, and monitoring

Read the most recent audit records from the running service:

```sh
docker compose exec -T server inboxd audit \
  --db /var/lib/agent-inbox/inbox.db --limit 100
```

Output is JSON in descending audit sequence. Records include accepted and
authenticated rejected sends, accepted and rejected acknowledgements, duplicate
sends, and unacknowledged escalations. Unauthenticated failures are not
persisted. Audit rows are append-only: SQLite triggers reject updates and
deletes. Restrict access to the database volume and audit output.

Monitor:

- `docker compose exec -T server inboxd healthcheck` and the Compose health
  state for listener and SQLite availability. Use a signed `agent-inbox poll` request
  to confirm an agent can reach the HTTP API; signed `GET /healthz` reports
  storage availability.
- `docker compose ps` health state and `docker compose logs server` for
  registry, storage, and webhook errors.
- Database volume and filesystem free space, backup age, and backup restore
  checks.
- Webhook delivery for accepted, rejected, acknowledged, and escalated events.
- Unacknowledged messages through the audit log and receiver poll. A
  notification or transport outage does not prove the agent is dead.

The health endpoint exposes only a status and storage result. Every HTTP
request is signed. Keep the service behind a TLS-terminating proxy when requests
cross an untrusted network.

## Troubleshooting common rejections

| Error code | Meaning and operator action |
| --- | --- |
| `unsigned_request` | The CLI did not attach signed request headers. Check `--agent`, `--key`, and that requests go through the supported CLI/protocol. |
| `invalid_signature` / `invalid_message_signature` | The key does not match the selected registry key, the body/path changed after signing, or a proxy rewrote the signed path/query. Preserve the API path and query exactly. |
| `stale_request` | Agent or server clock differs beyond `INBOX_REQUEST_SKEW`; synchronize clocks. |
| `replayed_request` | The same nonce was reused. Generate a fresh signed request; message retries should use the same message ID with a fresh HTTP request nonce. |
| `unregistered_agent` / `unknown_key` | Add the reviewed public key or use an active registered key ID. A removed or disabled registry entry is revoked. |
| `recipient_not_allowed` | The sender's `allowed_recipients` list does not include the explicit recipient. Review and update the registry. |
| `kind_not_allowed` | The sender is not allowed to send that kind. Check the registry's `allowed_kinds`. |
| `unknown_recipient` | The recipient is absent, disabled, or has no active public key. Confirm its registry entry. |
| `wrong_tenant` | Sender and recipient have different registry tenants. Do not accept a tenant from the caller; review the human-approved assignment. |
| `secret_detected` | A payload, provenance field, ack note, or artifact reference matched a common key/token/private-key pattern. Remove the secret and rotate it if it was exposed elsewhere. |
| `reply_to_required` / `reply_correlation_mismatch` | A result must refer to the addressed message and reuse its task and thread IDs. |
| `message_id_conflict` | The ID already belongs to different content. Generate a new message ID; use an existing ID only for an identical retry. |
| `storage_unavailable` | SQLite could not read or persist state. Check container health, volume permissions, disk space, and logs. |
| `registry_unavailable` | Registry JSON is unreadable or invalid. Restore the last valid reviewed file; malformed registry edits fail closed. |

## Moving storage to PostgreSQL later

Only SQLite ships in the pilot. The handlers depend on the `Store` interface in
`internal/inbox/store.go`; SQLite-specific schema and SQL are isolated in
`SQLiteStore`. A PostgreSQL implementation should preserve the same observable
contract: transactionally assign monotonic sequence numbers, unique message
IDs and request nonces, atomically append audit rows with accepted writes, keep
audit append-only, and return unacknowledged messages in order.

The planned cutover is:

1. Implement and test a PostgreSQL `Store` with equivalent migrations and
   constraints.
2. Add connection settings and a backend selection in the store factory. The
   deployment can then switch `INBOX_STORAGE=postgres` plus a PostgreSQL
   connection string; this pilot deliberately rejects that setting because no
   PostgreSQL driver ships here.
3. Take a consistent SQLite snapshot with `inboxd backup`, import messages,
   tenant assignments, sequence values, ack state, nonces still inside the
   replay window, notification state, and the full audit log.
4. Stop sends for cutover, compare counts and high-water sequences, switch the
   backend configuration, then verify polls, ack, and audit before reopening
   traffic.
5. Keep the SQLite snapshot for rollback until the PostgreSQL service has
   passed restore and operational checks.

Do not run SQLite and PostgreSQL as competing active inboxes during cutover;
that would break message ID, sequence, nonce, and audit guarantees.
