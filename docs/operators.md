# Operator guide

This guide covers operating the standalone inbox. Operators manage it through
the reviewed registry plus audit and monitoring commands; agents have no
registry or administrative endpoint. Human-authored requests use a separate
organization-owned issue-tracker form, which is outside this pilot. The service
accepts signed machine requests and sends human-visible event notifications
through a generic webhook. Local Compose uses a bundled sink that logs
notification events; production must configure a human-operated webhook.

## Requirements and first start

- Docker Engine with the Compose plugin.
- A registry file readable by the service. Start from the empty
  [`config/registry.json`](../config/registry.json).
- A reachable webhook URL for human-visible notifications. The local Compose
  default is the bundled notification sink; production must set
  `INBOX_WEBHOOK_URL` to a human-operated endpoint. Remote webhooks must use
  HTTPS; plain HTTP is limited to loopback IPs, `localhost`, and the exact
  bundled Compose hostname `notification-sink`.
- A TLS-terminating reverse proxy for use outside a trusted local test. The
  service itself listens on plain HTTP and does not manage certificates.

1. Clone this repository and enter its directory.
2. Install or build the `agent-inbox` CLI on operator and agent workstations,
   following [the agent guide](agents.md). The server image contains `inboxd`
   only. Have each agent operator generate an Ed25519 keypair with the CLI and
   provide only the public key through a human-approved channel.
3. Edit `config/registry.json` to add the approved agents and their allowed
   recipients and message kinds. See [Registry operations](#registry-operations).
4. Copy the environment example, replace the local sink URL with a reachable
   human-operated webhook for production, then start Compose:

   ```sh
   cp .env.example .env
   # For production, edit .env to set INBOX_WEBHOOK_URL.
   docker compose up -d --build
   docker compose ps
   ```

   The local sink accepts webhook events and writes them to the
   `notification-sink` service log. It is intended for local testing; production
   must use a human-operated webhook endpoint.

5. Confirm the local listener and selected storage health check, then inspect service logs:

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
| `INBOX_STORAGE` | `sqlite` | Storage backend: `sqlite` or `postgres`. |
| `INBOX_DB_PATH` | `/var/lib/agent-inbox/inbox.db` | SQLite database path. |
| `INBOX_DATABASE_URL` | unset | PostgreSQL connection URL; required when `INBOX_STORAGE=postgres`. |
| `INBOX_REGISTRY_PATH` | `/etc/agent-inbox/registry.json` | Human-managed JSON registry path. |
| `INBOX_WEBHOOK_URL` | `http://notification-sink:8081/notifications` in Compose | Webhook for consequential human-visible events. Remote hosts require HTTPS; HTTP is limited to loopback IPs, `localhost`, and the exact bundled Compose hostname `notification-sink`. Required at startup; use a human-operated endpoint in production. |
| `INBOX_RETRY_INTERVAL` | `30s` | Delay between doorbell attempts and before unacknowledged escalation. Accepts Go duration syntax with a minimum of `1s`. |
| `INBOX_MAX_DOORBELL_ATTEMPTS` | `3` | Maximum scheduled doorbell attempts, including the initial ring, before one escalation. Tenant reassignment suppresses rings but does not cancel the schedule. |
| `INBOX_REQUEST_SKEW` | `5m` | Maximum difference between a signed request timestamp and server time; maximum `12h` to match nonce retention. Accepts Go duration syntax. |
| `INBOX_HOST_PORT` | `8080` | Host loopback port published by Compose. |
| `INBOX_REGISTRY_FILE` | `./config/registry.json` | Host path mounted read-only as the registry. |

For PostgreSQL, use `compose.postgres.yaml` with the base file and configure
`POSTGRES_PASSWORD`, `INBOX_HOST_PORT`, and `INBOX_WEBHOOK_URL` when using a
production webhook. The local Compose configuration connects to the bundled
notification sink. The example PostgreSQL service is suitable for a pilot on
one host; production deployments should use their managed or separately
operated PostgreSQL service and a protected `INBOX_DATABASE_URL`.

The Compose fallback inserts `POSTGRES_PASSWORD` into a PostgreSQL URL. Keep
that password URI-safe, or set `INBOX_DATABASE_URL` explicitly when it contains
reserved URL characters such as `/`, `?`, `#`, `%`, or `@`. Keep the raw
password in `POSTGRES_PASSWORD` for the database container and percent-encode
it only in the URL's password component.
For example, the raw password `p/a?b#c%d` needs this server URL:
`postgres://agent_inbox:p%2Fa%3Fb%23c%25d@postgres:5432/agent_inbox?sslmode=disable`.

Each webhook event includes its event name, timestamp, message ID, agent IDs,
tenant, kind, or rejection code as applicable. It never includes message
payloads or key material. Events include `message.accepted`,
`message.acknowledged`, `message.rejected`, and
`message.unacknowledged_escalation`. Webhook delivery is best-effort and each
event is attempted once after its consequence is persisted. A delivery failure
adds a `notification.failed` audit entry with the event, message ID when
available, and error class; it does not undo a message or acknowledgement or
change the API response. Doorbell retries remain separate from webhook
delivery. The escalation state and audit entry are committed before its single
webhook attempt. Webhook failure logs include only the endpoint scheme and
host; URL userinfo, path, and query parameters are omitted.

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
Replace the file and confirm requests signed by the old credentials receive
`authentication_failed`. The periodic server log reports aggregate counts by
authentication failure reason. Revocation affects new requests immediately; it
does not delete already accepted messages or audit history. Open doorbell
streams are closed at the next heartbeat after their signing key is revoked.

Changing an agent's tenant immediately isolates it from messages stored under
its previous tenant. Those messages remain in the audit and storage history but
no longer appear in that agent's polls, can no longer be acknowledged by it, and
will not trigger further doorbells to it. An unacknowledged message still follows
the bounded notification schedule and escalates once to the operator webhook.

## Signing and delivery behavior

Every HTTP request to the inbox service requires Ed25519 request headers,
including requests to unknown paths. An authenticated request to an unknown
path returns 404. The signed request canonical bytes are the UTF-8 text:

```text
agent-inbox-request-v1\n<agent-id>\n<key-id>\n<METHOD>\n<exact-path-and-query>\n<unix-seconds>\n<nonce>\n<lowercase-hex-sha256-of-body>
```

The signed nonce is stored with a uniqueness constraint and the timestamp must
be within the configured skew. Each message has a second signature. Message
signing bytes are `agent-inbox-envelope-v1\n` followed by compact JSON of the sender-authored
fields, in the order `type`, `id`, `task_id`, `thread_id`, optional `reply_to`,
`sender_id`, `recipient_id`, `key_id`, `kind`, `created_at`, optional
`asserted_authority`, `content_is_data`, `payload`, `artifacts`, and optional
`provenance`. The `signature`, server tenant, sequence, acceptance time, and
acknowledgement state are excluded. Object keys in payload and provenance are sorted by
Go's `encoding/json`; array order and JSON number representation are
significant. Payloads and artifacts are signed as data.

The server commits a new message and its accepted-send audit record before it
publishes a doorbell. The SSE stream is in-memory and can lose events; poll is
the delivery method. The receiver should ack only after processing. A retry
from the same sender with the same message ID returns the original sequence
when the server-derived tenant and all sender-authored envelope fields match,
except `created_at`, `key_id`, and the resulting message signature. Reusing
that ID with any other change is a conflict. Each retry still needs a valid
message signature and a fresh signed-request nonce. A result must cite the
original message and match its sender, recipient, task, and thread. A transport
failure means delivery is unknown, not that an agent is dead.

New message IDs must be UUID v4, UUID v7, or ULID. Previously stored UUID IDs
remain usable for retries, replies, and acknowledgements. Treat message IDs as
capability-like references and share them only with authorized participants. A
send using an occupied ID can reveal that it is already in use, so only submit
IDs an operator or agent generated or otherwise already knows. Acknowledgement
requests for missing, inaccessible, or wrong-tenant messages all return the
same `message_not_found` response; the audit record keeps the internal reason.

The signed JSON API is:

| Method and path | Behavior |
| --- | --- |
| `POST /v1/messages` | Submit a signed envelope to its explicit `recipient_id`. |
| `GET /v1/messages?limit=N` | Poll the authenticated agent's unacknowledged inbox in sequence order. The optional limit is bounded; no cursor or acknowledgement-history mode is available. |
| `POST /v1/messages/{id}/ack` | Acknowledge a message after processing with `{"processed":true}`; the server records the acknowledgement time. |
| `GET /v1/events` | Open an authenticated SSE doorbell stream for the current agent. |

The service marks every HTTP response `Cache-Control: no-store`; SSE responses
also retain the `no-cache` directive. Preserve these headers through any proxy.

Signed API requests carry `X-Agent-ID`, `X-Key-ID`, `X-Request-Timestamp`,
`X-Request-Nonce`, and `X-Request-Signature` headers. The signature covers the
exact path and query, so reverse proxies must preserve them. Rejected
pre-authentication requests do not create audit rows or webhook events. Bounded
in-memory counters are available only in the periodic operator log, once per
minute when nonzero; the counters reset when the server restarts. They aggregate
unauthenticated rejections because row-by-row audit entries would let
unauthenticated requests grow the audit store without bound.

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
  --out /var/lib/agent-inbox/agent-inbox-backup.db
container_id="$(docker compose ps -q server)"
docker cp "$container_id:/var/lib/agent-inbox/agent-inbox-backup.db" ./agent-inbox-backup.db
```

The snapshot is written to the persistent database volume, not the container's
16 MiB `/tmp` filesystem. Confirm the volume has room for a database-sized
snapshot before starting the backup.

Store backups encrypted and access-controlled. The database may contain
operator-supplied content, tenant identifiers, and message metadata. Keep
enough dated copies to meet the deployment's recovery objective and verify
restores periodically.

To restore a snapshot, stop the service and use a small utility container to
replace the file in the named volume. The server image is `scratch` and has no
shell, so the helper uses Alpine:

```sh
docker compose stop server
backup="$(pwd -P)/agent-inbox-backup.db"
container_id="$(docker compose ps -aq server)"
volume="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/agent-inbox"}}{{.Name}}{{end}}{{end}}' "$container_id")"
docker run --rm \
  -v "$volume:/data" \
  -v "$backup:/restore.db:ro" \
  alpine:3.21 sh -ec '
    rm -f /data/inbox.db-wal /data/inbox.db-shm
    cp /restore.db /data/inbox.db.restore
    chown 65532:65532 /data/inbox.db.restore
    chmod 0600 /data/inbox.db.restore
    mv /data/inbox.db.restore /data/inbox.db
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

The Compose named volume survives container replacement. The server initializes
tables idempotently and applies documented compatibility migrations at startup.
This release removes legacy acknowledgement metadata columns from SQLite; make a
backup first and do not expect an older binary to use the migrated database.
Keep the prior image available until health and audit checks pass. Restore a
backup to roll back across an incompatible schema change.

## Audit, health, and monitoring

Read the most recent audit records from the running service:

```sh
docker compose exec -T server inboxd audit \
  --limit 100
```

Output is JSON in descending audit sequence. Records include accepted and
authenticated rejected sends, accepted and rejected acknowledgements, duplicate
sends, unacknowledged escalation attempts, and failed webhook deliveries.
Unauthenticated failures are not persisted. Audit rows are append-only: both
backends reject updates and deletes with database triggers. Restrict access to
the database and audit output.

Monitor:

- `docker compose exec -T server inboxd healthcheck` and the Compose health
  state for listener and selected storage availability. Use a signed
  `agent-inbox poll` request to confirm an agent can reach the API.
- `docker compose ps` health state and `docker compose logs server` for
  registry, storage, and webhook errors.
- Database volume and filesystem free space, backup age, and backup restore
  checks.
- Webhook delivery, the local `notification-sink` log, and `notification.failed`
  audit entries for accepted, rejected, acknowledged, and escalated events.
  Production must configure a human-operated webhook.
- Unacknowledged messages through the audit log and receiver poll. A
  notification or transport outage does not prove the agent is dead.

The container's `inboxd healthcheck` opens and pings the configured database,
then checks the listener's TCP port. The inbox service has no HTTP health
endpoint; every HTTP request requires a signature. Keep the service behind a
TLS-terminating proxy when requests cross an untrusted network.

## Troubleshooting common rejections

| Error code | Meaning and operator action |
| --- | --- |
| `authentication_failed` | The request could not be authenticated. This response intentionally does not distinguish an unknown or revoked agent, unknown key, invalid request signature, stale timestamp, invalid nonce, replay, or registry/replay-store failure. Check the periodic authentication failure counts in `docker compose logs server`; confirm the agent, key ID, signature inputs, clocks, registry, and database configuration. |
| `invalid_message_signature` | The HTTP request was authenticated, but the message envelope signature is invalid. Check that the envelope was signed by the active key and was not changed afterward. |
| `request_too_large` | The request exceeds the 64 KiB limit. Reduce the request body. |
| `invalid_task_id` / `invalid_thread_id` | A new instruction needs UUID v4, UUID v7, or ULID correlation IDs. Results should copy the exact task and thread IDs from the instruction they answer. |
| `recipient_not_allowed` | The recipient is absent, inactive, or not in the sender's `allowed_recipients` list. The client response intentionally hides which condition applies; review the registry and server audit records. |
| `kind_not_allowed` | The sender is not allowed to send that kind. Check the registry's `allowed_kinds`. |
| `wrong_tenant` (audit only) | The sender and recipient have different registry tenants. The client receives `recipient_not_allowed`; review the human-approved assignment. |
| `secret_detected` | A payload, provenance field, or artifact reference matched a common key/token/private-key pattern. Remove the secret and rotate it if it was exposed elsewhere. |
| `unsupported_query_parameter` | Poll accepts only `limit`. Remove old cursor or history parameters and poll the inbox again. |
| `reply_to_required` / `invalid_reply_to` | A result must refer to a message addressed to its sender and reuse its task and thread IDs. The client response does not reveal whether a reference is missing, inaccessible, or mis-correlated; inspect the audit record for the internal reason. |
| `message_id_conflict` | The ID already belongs to different content. Generate a new message ID; use an existing ID only for an identical retry. |
| `message_not_found` | The acknowledgement target is missing or not accessible to this agent. The client response hides which condition applies; inspect the audit record for the internal reason. |
| `storage_unavailable` | The configured database could not read or persist state. Check container health, connection settings, volume permissions, disk space, and logs. |
| `registry_unavailable` | Registry JSON is unreadable or invalid. Restore the last valid reviewed file; malformed registry edits fail closed. |

## Moving storage to PostgreSQL

The server includes a PostgreSQL `Store` implementation. Both backends use the
same handlers and delivery behavior. The application owns schema initialization
and append-only audit protections. For an all-in-one Compose pilot, start the
PostgreSQL profile with the base service file:

```sh
export POSTGRES_PASSWORD='replace-with-a-long-random-password'
export INBOX_HOST_PORT=8080
export INBOX_WEBHOOK_URL='https://notify.example.com/agent-inbox'
docker compose -f compose.yaml -f compose.postgres.yaml up -d --build
docker compose -f compose.yaml -f compose.postgres.yaml ps
docker compose -f compose.yaml -f compose.postgres.yaml exec -T server inboxd healthcheck
```

If `POSTGRES_PASSWORD` contains reserved URL characters, also export
`INBOX_DATABASE_URL` using the percent-encoded password, for example:

```sh
export POSTGRES_PASSWORD='p/a?b#c%d'
export INBOX_DATABASE_URL='postgres://agent_inbox:p%2Fa%3Fb%23c%25d@postgres:5432/agent_inbox?sslmode=disable'
```

For production, use a separately managed PostgreSQL service and set
`INBOX_STORAGE=postgres` and `INBOX_DATABASE_URL` on the server. Keep the URL
out of source control and shell history. The Compose example disables TLS on
the private Compose network; configure TLS for remote PostgreSQL connections.
`inboxd backup` is SQLite-only. Back up PostgreSQL with `pg_dump`. Restore only
after stopping the inbox server; `--clean` replaces existing database objects:

```sh
docker compose -f compose.yaml -f compose.postgres.yaml exec -T postgres \
  sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > agent-inbox.dump
docker compose -f compose.yaml -f compose.postgres.yaml stop server
docker compose -f compose.yaml -f compose.postgres.yaml exec -T postgres \
  sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists' \
  < agent-inbox.dump
docker compose -f compose.yaml -f compose.postgres.yaml up -d server
docker compose -f compose.yaml -f compose.postgres.yaml exec -T server inboxd healthcheck
```

### SQLite to PostgreSQL cutover

Use a maintenance window so both stores never accept writes at the same time.
Set the reverse proxy to reject new requests, wait for active sends and
acknowledgements to finish, then take an online SQLite snapshot with the
`inboxd backup` command above. Leave the SQLite server stopped while importing
and verifying the snapshot. Start the PostgreSQL Compose profile once to create
its empty schema, then stop its server before import. Use `sqlite3` and `psql`
clients on the operator machine; `INBOX_DATABASE_URL` must be reachable from
that machine. Export/import the four data tables in dependency order:

```sh
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT sequence,id,sender_id,recipient_id,tenant_id,kind,task_id,thread_id,reply_to,envelope_json,accepted_at,acknowledged_at FROM messages ORDER BY sequence' > messages.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT agent_id,nonce,created_at FROM request_nonces' > request_nonces.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT sequence,occurred_at,action,actor_id,subject_id,tenant_id,outcome,code,detail_json FROM audit_log ORDER BY sequence' > audit_log.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT message_id,attempts,last_notified_at,escalated_at FROM notification_state' > notification_state.csv
```

Set `INBOX_DATABASE_URL` to the PostgreSQL target, then import with `psql` in
the same directory. For the Compose example, the host-side URL uses its
loopback-published port, for example:

```sh
export INBOX_DATABASE_URL='postgres://agent_inbox:replace-with-a-long-random-password@127.0.0.1:15432/agent_inbox?sslmode=disable'
```

Use the password configured for that Compose project and URL-encode reserved
characters in it. Then import:

```sh
psql "$INBOX_DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
\copy messages(sequence,id,sender_id,recipient_id,tenant_id,kind,task_id,thread_id,reply_to,envelope_json,accepted_at,acknowledged_at) FROM 'messages.csv' CSV HEADER
\copy request_nonces(agent_id,nonce,created_at) FROM 'request_nonces.csv' CSV HEADER
\copy audit_log(sequence,occurred_at,action,actor_id,subject_id,tenant_id,outcome,code,detail_json) FROM 'audit_log.csv' CSV HEADER
\copy notification_state(message_id,attempts,last_notified_at,escalated_at) FROM 'notification_state.csv' CSV HEADER
SELECT setval(pg_get_serial_sequence('messages','sequence'), COALESCE((SELECT MAX(sequence) FROM messages), 1), EXISTS(SELECT 1 FROM messages));
SELECT setval(pg_get_serial_sequence('audit_log','sequence'), COALESCE((SELECT MAX(sequence) FROM audit_log), 1), EXISTS(SELECT 1 FROM audit_log));
SQL
```

Compare row counts and maximum message/audit sequence values between the
snapshot and PostgreSQL. Run `inboxd audit --limit 100`, verify known agent
polls and acknowledgements, and inspect notification state before allowing the
proxy to reopen traffic. Keep the SQLite snapshot intact until PostgreSQL
backup and restore have been verified. Never operate both databases as active
inboxes during or after cutover; that would split ordering, replay protection,
deduplication, and audit history.
