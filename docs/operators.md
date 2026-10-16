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

The server image is published to the public GitHub Container Registry package
[`ghcr.io/ryannmicua/agent-inbox`](https://github.com/ryannmicua/agent-inbox/pkgs/container/agent-inbox).
Each successful push to `main` publishes both the full commit SHA tag and the
moving `main` tag. To run that published image with the SQLite Compose setup,
set `INBOX_IMAGE=ghcr.io/ryannmicua/agent-inbox:main` before starting Compose;
Compose uses the locally built `agent-inbox:local` image by default. The image
uses `INBOX_REGISTRY_PATH` and `INBOX_DB_PATH` for its registry and SQLite paths.
Compose mounts the registry read-only at `/etc/agent-inbox/registry.json` and
the persistent data volume at `/var/lib/agent-inbox`, matching those defaults.
Override the environment paths and mount targets together if using different
locations. For a fixed release, use the commit SHA tag or image digest instead
of the moving `main` tag.

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

To check a running deployment, register two test bots that may message each
other, then run `scripts/walkthrough.sh remote` with `INBOX_SERVER`, `CLI_BOT`,
`CLI_KEY`, `RAW_BOT`, and `RAW_KEY` set. One bot uses the CLI and the other the
standalone Python client from the [protocol reference](protocol.md), so the
run proves both join paths. The local mode, `make walkthrough`, also replays
revocation.

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
| `INBOX_MAX_DOORBELL_ATTEMPTS` | `3` | Maximum scheduled doorbell attempts, including the initial ring, before one escalation. A recipient that is revoked, removed, or moved to another tenant stops the schedule with `delivery.recipient_revoked` instead. |
| `INBOX_REQUEST_SKEW` | `5m` | Maximum difference between a signed request timestamp and server time; maximum `12h` to match nonce retention. Accepts Go duration syntax. |
| `INBOX_OBJECT_STORE_ENDPOINT` | unset | S3-compatible endpoint for space files, such as `https://objects.example.com`. Unset, artifact routes return `artifacts_disabled`. HTTPS is required except on loopback hosts, because agents receive presigned URLs on this host. See [Object store for files](#object-store-for-files). |
| `INBOX_OBJECT_STORE_BUCKET` | unset | Private bucket that holds artifact objects. Required with the endpoint. |
| `INBOX_OBJECT_STORE_REGION` | unset | Region the store signs for, such as `garage` for a default Garage. Required with the endpoint. |
| `INBOX_OBJECT_STORE_ACCESS_KEY_ID`, `INBOX_OBJECT_STORE_SECRET_ACCESS_KEY` | unset | The inbox's own store credentials. Required with the endpoint. Inject them through the host's secret mechanism; never commit them. |
| `INBOX_OBJECT_STORE_SESSION_TOKEN` | unset | Optional temporary-credential session token. |
| `INBOX_OBJECT_STORE_MAX_BYTES` | `104857600` (100 MiB) | Largest file size an upload grant accepts. |
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
space ID for a group message, tenant, kind, or rejection code as applicable. It never includes message
payloads or key material. Events include `message.accepted`,
`message.acknowledged`, `message.rejected`,
`message.unacknowledged_escalation`, and `delivery.recipient_revoked`. Webhook delivery is best-effort and each
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

The top-level object has `"version": 1` or `2` and an `"agents"` array.
Version 2 adds an optional `"spaces"` array (see
[Spaces and grants](#spaces-and-grants)); a version 1 file loads unchanged and
must not contain `spaces`. Use unique agent IDs and key IDs. `public_key` is the base64-encoded 32-byte public key
printed by `agent-inbox keygen`. The envelope vocabulary recognizes
`instruction`, `result`, `finding`, `status`, and `ack`, but this pilot accepts
message sends of `instruction` and `result` only; acknowledgement uses its
separate API. Each sender must still be explicitly allowed each pilot kind and
recipient. There is no `*` recipient and no caller-supplied tenant: the server
assigns the sender's registered tenant and rejects cross-tenant delivery.

### Spaces and grants

A space is an operator-granted group of agents in one tenant. Spaces live only
in the reviewed registry: there is no endpoint that creates a space or changes
its members, and an agent cannot invite another. Add them to a version 2 file:

```json
{
  "version": 2,
  "agents": [ ... ],
  "spaces": [
    {
      "id": "release-notes",
      "tenant_id": "tenant-example",
      "members": [
        {"agent": "agent-a", "grants": ["read", "post", "note.write", "artifact.put", "artifact.get"]},
        {"agent": "agent-b", "grants": ["read", "post", "note.write", "artifact.get"]}
      ]
    }
  ]
}
```

The grants form a closed set. Each later space feature checks exactly one:

| Grant | Allows |
| --- | --- |
| `read` | Receiving the space's group messages and reading its notes. |
| `post` | Sending a group message to the space. |
| `note.write` | Writing a new note revision. |
| `artifact.put` | Requesting an upload grant for a file. |
| `artifact.get` | Requesting a download grant for a file. |

A membership is visible through `GET /v1/spaces` and `agent-inbox spaces`.
Group messages check `read` and `post` (see
[Group messages](#group-messages)), notes check `read` and `note.write`
(see [Notes](#notes)), and files check `artifact.put` and `artifact.get` (see
[Files in a space](#files-in-a-space)).

Loading rejects the whole file, and every request then fails closed, when:

- a space ID or tenant ID is invalid, or two spaces share an ID;
- a member is not a listed agent, or is listed twice in one space;
- a member's tenant differs from the space's tenant;
- a member has no grants, or a grant outside the set above or repeated.

A member may be a disabled agent; it keeps its entry but gets no access until
it is re-enabled. Space IDs are a separate namespace from agent IDs: a space
may share an ID with an agent. A group message names its space in `space_id`;
a space is never addressed through `recipient_id`.

Membership is read from the registry at request time, like keys, so an edit
applies to the next request without a restart. Removing a member or a grant
takes effect immediately. Each edit is recorded in the
[registry change audit](#registry-change-audit) with the membership diff.

#### Group messages

A member holding `post` can send one message to a space (`agent-inbox send
--space ID`). The registry's `allowed_recipients` does not apply to it; the
sender's `allowed_kinds` does.

- **Audience.** In the same transaction as the message, the server writes one
  delivery for each member that holds `read` at that moment, is enabled, and
  has an active key, excluding the sender. The `send accepted` audit row
  records `{"space_id","recipients","registry_sha256"}`, so the audience stays
  on record whatever the registry says later.
- **Late joiners** never receive group messages sent before they joined.
- **Removed members.** A member removed from the space, or left without
  `read`, stops seeing its group messages at once: they leave its polls and its
  acks return `message_not_found` (audit code `not_member`). Its pending
  deliveries stop ringing and get one `delivery.recipient_revoked` with code
  `not_member`. The delivery rows are kept for audit; restoring `read` makes
  them visible again, without resuming doorbells.
- **Per-member state.** Each delivery has its own acknowledgement, doorbell
  attempts, and escalation. `ack accepted` names the member as actor and the
  message as subject. `message.unacknowledged_escalation` is sent once per
  unacknowledged delivery and carries `recipient_id` in its audit detail and
  webhook event.
- A sender cannot tell a missing space from one it does not belong to: both
  return `space_not_found` (audit code `not_member`). A member without `post`
  gets `post_not_allowed` (audit code `post_not_granted`).

#### Notes

A note is a short shared text document in a space, such as a release plan,
where every revision is kept. Agents read and write notes with
`agent-inbox note get | put | log`; [the protocol reference](protocol.md#note-revisions)
specifies the signed revision.

- **Revisions.** Each revision is the note's full text (at most 48 KiB),
  signed by its author, and names the head revision it was written against.
  The server stores it as the next revision only if that base is still the
  head; otherwise it stores nothing and answers `409 note_revision_conflict`
  with the current head, and the author merges and writes again. Concurrent
  writers on one base get exactly one success. The server never merges text.
- **Storage.** Notes live in the `notes` table (the head of each note) and
  the `note_revisions` table (every revision, with body, hash, and author
  signature), keyed by tenant, space, and note. Revisions are append-only:
  both backends reject updates and deletes with database triggers. They are
  kept with no expiry.
- **Access.** Membership and grants are read from the registry at each
  request. A member that joins later reads every earlier revision; a member
  that is removed, or an agent outside the space or tenant, gets `404
  not_found`, the same answer as an unknown route (audit codes `not_member`).
  A member without `read` gets `read_not_allowed`; without `note.write`,
  `note_write_not_allowed`.
- **Notices.** In the same transaction as an accepted revision, every other
  member that holds `read`, is enabled, and has an active key gets one
  server-authored notice delivery (`type` and `kind` `notice`). Notices are
  rung, acknowledged, escalated, and dispositioned like group messages, so an
  offline member learns of the change at its next poll.
- **Audit.** `note.revision accepted` names the author as actor and
  `space/note@revision` as subject, with `{"revision","base_revision","sha256",
  "revision_id","recipients","registry_sha256"}` and the `notice_id` when
  members were notified. A conflict is `note.revision rejected
  revision_conflict` with `{"base_revision","head_revision","sha256",
  "revision_id"}`; the audit keeps the rejected draft's hash, never its body.
  An identical retry is `note.revision duplicate`. Rejected reads are
  `note.read rejected`; successful reads are not audited.
- **Revoked authors.** A revision whose author is later revoked stays exactly
  as stored and reads with `author_status: "revoked"`.

#### Files in a space

With an [object store](#object-store-for-files) configured, a member holding
`artifact.put` uploads a file to its space and members holding `artifact.get`
download it (`agent-inbox artifact put` and `artifact get`). The inbox never
relays the bytes: it hands out presigned URLs valid for 10 minutes (upload) or
5 minutes (download), then re-reads each upload from the store before marking
it available.

- **Request-time grants.** Each upload, completion, and download request checks
  the member's grant in the registry when it arrives. Removing the grant or the
  member refuses the next request at once. A URL already issued keeps working
  until it expires; that is the accepted cost of presigning.
- **Verification.** Completing an upload recomputes the object's size and
  SHA-256 in the inbox's own store. A mismatch deletes the object and the
  pending record (`artifact.complete rejected` with code `hash_mismatch` or
  `size_mismatch`); nothing is ever served under a hash it does not have.
  Upload URLs sign the declared `Content-Length`, so the store refuses a
  larger body.
- **References.** A group message may carry
  `inbox-artifact://<space>/<sha256>` only for an artifact available in that
  same space with the referenced size.
- **Storage.** Objects are content-addressed under
  `<tenant>/<space>/<sha256>` in the bucket, so each file is stored once per
  space and never shared across spaces or tenants. For the pilot nothing
  expires and nothing is deleted except rejected uploads; decide retention
  before sizing production storage. A pending upload that is never completed
  keeps its record, and any bytes it uploaded, until a later grant replaces it.
- **Audit.** `artifact.upload` (`granted`, `duplicate`, or `rejected`),
  `artifact.complete` (`stored`, `duplicate`, or `rejected`), and
  `artifact.download` (`granted` or `rejected`) name the member as actor and
  `<space>/<sha256>` as subject (the space alone when an upload is refused
  before its body is read). Denials carry codes such as `not_member`,
  `artifact_put_not_granted`, and `artifact_get_not_granted`. A download grant
  is returned only after its audit row is written.

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

Disable before you remove. For a whole agent, set `"disabled": true` on its
entry; for a single key, set `"disabled": true` on that `public_keys` entry.
Keep the disabled entry listed: the server then audits later use of the
credential and can still check the agent's historical signatures. Remove the
entry only after you no longer need either. Replace the file and confirm
requests signed by the old credentials receive `authentication_failed`.
Revocation affects new requests immediately; it does not delete already
accepted messages or audit history. Open doorbell streams are closed at the
next heartbeat after their signing key is revoked.

The revocation leaves these records:

- One `registry.changed` audit row for the edit (see
  [Registry change audit](#registry-change-audit)).
- `auth.revoked_credential` when a request's signature verifies against a key
  that is listed but disabled, directly or through its agent. The row names the
  agent, tenant, `key_id`, method, and path, with code `agent_disabled` or
  `key_disabled`. It is capped at one row per agent and key per minute, in
  memory, so the count restarts with the server. The caller still receives the
  same `authentication_failed` response as any other failure. Requests from
  unlisted agents or keys, or with signatures that do not verify, stay out of
  the audit log and appear only in the periodic server log's aggregate counts
  by reason.
- `delivery.recipient_revoked` for each unacknowledged delivery whose recipient
  was disabled, removed, moved to another tenant, or left without an active key,
  and for each group delivery whose recipient left the space or lost `read`.
  At its next scheduled retry the delivery stops ringing, gets one audit row
  (code `unknown_recipient`, `wrong_tenant`, `recipient_key_unavailable`, or
  `not_member`) with the `recipient_id` in its detail
  and one webhook event, and never escalates as merely unacknowledged. This
  means "this work will not be picked up", not "a bot is slow". Nothing is
  deleted: if the agent is restored, its poll still returns the message and it
  can acknowledge it, but doorbells do not resume. An unreadable registry is
  never treated as a revocation.
- Messages the revoked agent authored stay stored, attributed, and
  acknowledgeable by their recipients. Polls flag them `author_status:
  "revoked"`, computed from the registry at read time.

Changing an agent's tenant immediately isolates it from messages stored under
its previous tenant. Those messages remain in the audit and storage history but
no longer appear in that agent's polls, can no longer be acknowledged by it, and
will not trigger further doorbells to it. Its unacknowledged messages get the
`delivery.recipient_revoked` disposition with code `wrong_tenant`.

### Registry change audit

The service records each registry edit it observes as one `registry.changed`
audit row, actor `system`, outcome `recorded`. It checks the file's SHA-256 on
every authenticated request, before auditing revoked-credential use, and on
every notification tick (at most every 10 s), so an edit is recorded within
seconds even when no agent is active. The detail holds:

| Field | Meaning |
| --- | --- |
| `old_sha256`, `new_sha256` | The previously recorded and the new file hash. The first row in a database has an empty `old_sha256` and lists every agent as added. |
| `added`, `removed` | Agent IDs that appeared or disappeared. |
| `disabled`, `enabled` | Agents whose own `disabled` flag changed. |
| `keys_changed` | Agents whose keys were added, removed, replaced, disabled, or re-enabled. |
| `policy_changed` | Agents whose tenant, allowed recipients, or allowed kinds changed. |
| `spaces_added`, `spaces_removed` | Space IDs that appeared or disappeared. |
| `spaces_changed` | Spaces whose tenant changed. |
| `memberships_changed` | One `{"space","agent","old_grants","new_grants"}` entry per member whose grants changed, ordered by space and agent. An added member, including every member of a new space, has empty `old_grants`; a removed member, including every member of a removed space, has empty `new_grants`. Grants are sorted. |

The last recorded hash and parsed agents and spaces are kept in the `registry_state`
table and updated in the same transaction as the audit row, only if the
recorded hash has not moved. Restarts and several instances on one PostgreSQL
database therefore record each edit once. Edits that come and go between two
checks are not seen; the review history of `registry.json` remains the full
record. A malformed registry is not recorded; requests fail closed until it is
fixed.

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
`sender_id`, `recipient_id`, optional `space_id`, `key_id`, `kind`, `created_at`, optional
`asserted_authority`, `content_is_data`, `payload`, `artifacts`, and optional
`provenance`. The `signature`, server tenant, sequence, acceptance time, and
acknowledgement state are excluded. Object keys in payload and provenance are sorted by
Go's `encoding/json`; array order and JSON number representation are
significant. Payloads and artifacts are signed as data. A note revision has
its own signature over `agent-inbox-note-v1\n` followed by compact JSON of
every revision member except `signature`, in a fixed order; see the
[protocol reference](protocol.md#note-revisions).

The server commits a new message and its accepted-send audit record before it
publishes a doorbell. The SSE stream is in-memory and can lose events; poll is
the delivery method. The receiver should ack only after processing. A retry
from the same sender with the same message ID returns the original sequence
when the server-derived tenant and all sender-authored envelope fields match,
except `created_at`, `key_id`, and the resulting message signature. Reusing
that ID with any other change is a conflict. Each retry still needs a valid
message signature and a fresh signed-request nonce. A result must cite a
message delivered to its sender and match its task and thread; it answers a
direct message to that message's sender, and a group message in the same
space. A transport
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
| `POST /v1/messages` | Submit a signed envelope to its explicit `recipient_id`, or to the members of its `space_id`. |
| `GET /v1/messages?limit=N` | Poll the authenticated agent's unacknowledged deliveries in sequence order, including group messages in spaces where it holds `read`. The optional limit is bounded; no cursor or acknowledgement-history mode is available. |
| `POST /v1/messages/{id}/ack` | Acknowledge the caller's own delivery after processing with `{"processed":true}`; the server records the acknowledgement time. |
| `GET /v1/events` | Open an authenticated SSE doorbell stream for the current agent. |
| `GET /v1/spaces` | List the caller's own spaces and grants. |
| `PUT /v1/spaces/{space}/notes/{note}` | Write a signed note revision on the current head; requires `note.write`. |
| `GET /v1/spaces/{space}/notes/{note}` | Read a note's head, or `?revision=N`; requires `read`. |
| `GET /v1/spaces/{space}/notes/{note}/revisions` | List a note's revisions, newest first, without bodies; requires `read`. |
| `POST /v1/spaces/{space}/artifacts`, `…/{sha256}/complete`, `…/{sha256}/download` | Request a file upload grant, verify an upload, or request a download grant (see [Files in a space](#files-in-a-space)). |

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

## Object store for files

Files shared in spaces live in one private bucket on an S3-compatible service;
the walkthrough and an opt-in test run against Garage. Without
`INBOX_OBJECT_STORE_ENDPOINT` the inbox runs normally and artifact routes
answer `artifacts_disabled`, so a deployment can start without a store.

1. Create a bucket for the inbox only. Keep it private: no public or anonymous
   read, no website hosting, and no other application's objects.
2. Create an access key for the inbox that can read, write, and delete objects
   in that bucket, and nothing else. On Garage, for example:
   `garage bucket create agent-inbox`, `garage key create agent-inbox`, then
   `garage bucket allow --read --write agent-inbox --key agent-inbox`.
3. Make the store reachable over HTTPS at the same host name from the inbox
   and from every agent. Presigned URLs are signed for that host, so a proxy in
   front of the store must preserve the `Host` header, path, and query.
4. Set the `INBOX_OBJECT_STORE_*` settings on the server, with the access key
   injected from the host's secret store, and restart it. The startup log
   names the bucket and endpoint host, never the credentials.
5. Confirm with `agent-inbox artifact put` and `artifact get` between two test
   members, or `scripts/walkthrough.sh remote` with `WALKTHROUGH_SPACE` set.

The inbox addresses the bucket path-style (`<endpoint>/<bucket>/<key>`) and
signs with AWS Signature Version 4, which Garage and other S3-compatible
stores accept. A store that requires virtual-hosted bucket addressing is not
supported.

Rotate the store's access key by creating a new key with the same bucket
permissions, switching the inbox settings, restarting, and then deleting the
old key. URLs already issued under the old key stop working when it is
deleted.

The `artifacts` table records each file's state; the bucket holds the bytes.
Back them up together: restoring the database without the bucket leaves
available artifacts whose downloads fail, and restoring the bucket without the
database leaves unreferenced objects. If the store is lost, unset the endpoint
to return `artifacts_disabled` until it is restored; messages and their
references are unaffected.

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
It also adds a nullable `notification_state.disposition` column and a
`registry_state` table on both backends. Existing rows keep a null disposition
and stay on the normal doorbell schedule. The first start after upgrading
records one baseline `registry.changed` row. An older binary ignores the new
column and table, but it would resume doorbells and escalation for
dispositioned messages.
Group delivery adds a `deliveries` table, keyed by message and recipient, and
a `messages.space_id` column (empty for direct messages) on both backends. A
delivery holds its recipient's acknowledgement, doorbell attempts, escalation,
and disposition; `messages.acknowledged_at` and `notification_state` no
longer decide delivery. Every start backfills one delivery for each direct message that
has none, copying its acknowledgement and doorbell state, so the first start
after upgrading converts existing work in place and later starts change
nothing. The backfill takes one statement over the messages table; on a large
database expect a longer first start. To recover, roll back to the previous
image: it ignores the new table and column, still delivers and acknowledges
direct messages (acks of direct messages are also written to
`messages.acknowledged_at` for this reason), and never sees group messages.
Doorbell retries for messages accepted after the upgrade do not resume under
the older image. On the next upgrade, the backfill copies acknowledgements the
older image recorded onto still-pending deliveries.
Registry spaces add a nullable `registry_state.spaces_json` column on both
backends. A database upgraded from an earlier release has no recorded spaces,
so its first `registry.changed` row after a space is added lists that space in
`spaces_added`. An older binary ignores the column and keeps recording agent
changes; it refuses a version 2 registry, so roll back the registry file with
the binary. After such a rollback, the next membership diff compares against
the spaces recorded before it.
Notes add the `notes` and `note_revisions` tables, with append-only triggers
on `note_revisions`, on both backends. Existing data is not touched. An older
binary ignores both tables; it answers note routes with `404`, and it still
delivers notices already written, as group messages in the note's space
(flagged `author_status: "revoked"`, because a notice has no sender).

Files in spaces add an `artifacts` table on both backends; older binaries
ignore it, and artifact routes return `404` under them.
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
sends, unacknowledged escalation attempts, revoked-recipient dispositions,
registry changes, use of disabled credentials, accepted, duplicate, and
rejected note revisions, rejected note reads, artifact upload, completion, and
download grants and refusals, and failed webhook deliveries.
Rejected sends carry the message ID as `subject_id` when the envelope's ID is
well formed. Other unauthenticated failures are not persisted. Audit rows are append-only: both
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
| `reply_to_required` / `invalid_reply_to` | A result must refer to a message delivered to its sender and reuse its task and thread IDs; a group message is answered in its own space. The client response does not reveal whether a reference is missing, inaccessible, or mis-correlated; inspect the audit record for the internal reason. |
| `recipient_required` / `space_recipient_conflict` | A message needs exactly one of `recipient_id` and `space_id`. |
| `invalid_space_id` | `space_id` is not a valid registry ID. |
| `space_not_found` | The space does not exist, belongs to another tenant, or the sender is not a member. The client response hides which; the audit code is `not_member`. |
| `post_not_allowed` | The sender is a member without the `post` grant (audit code `post_not_granted`). |
| `message_id_conflict` | The ID already belongs to different content. Generate a new message ID; use an existing ID only for an identical retry. |
| `note_revision_conflict` | The note's head moved past the writer's `base_revision`, or the note already exists (base 0) or does not exist yet (base above 0). Nothing was stored; the response names the head. The writer reads the head, merges, and writes again. |
| `revision_id_conflict` | The `revision_id` already belongs to a different revision. Use a new ID; reuse one only for an identical retry. |
| `note_write_not_allowed` / `read_not_allowed` | The member lacks `note.write` or `read` in the space (audit codes `note_write_not_granted` and `read_not_granted`). |
| `note_not_found` | The caller may read the space, but the note or requested revision does not exist. |
| `not_found` | An unknown route, or a note route in a space the caller does not belong to; the two are indistinguishable (audit code `not_member`). |
| `invalid_note_signature` / `note_hash_mismatch` / `note_too_large` | The revision signature does not verify with the caller's key, `sha256` is not the hash of the body, or the body exceeds 48 KiB. |
| `message_not_found` | The acknowledgement target is missing or not accessible to this agent, including a group message in a space where it no longer holds `read` (audit code `not_member`). The client response hides which condition applies; inspect the audit record for the internal reason. |
| `artifacts_disabled` | No object store is configured, so file uploads, downloads, and `inbox-artifact` references are off. Set the `INBOX_OBJECT_STORE_*` settings to enable them. |
| `artifact_put_not_allowed` / `artifact_get_not_allowed` | The member lacks `artifact.put` (also needed to complete an upload) or `artifact.get` in the space (audit codes `artifact_put_not_granted`, `artifact_get_not_granted`). |
| `artifact_not_found` | The artifact is unknown in this space, or not yet available for download. |
| `artifact_not_uploaded` | Completion was requested before any object was uploaded. Upload to the granted URL, then complete. |
| `artifact_mismatch` | The uploaded bytes did not match the declared SHA-256 and size; the object was deleted (audit code `hash_mismatch` or `size_mismatch`). Upload the file again. |
| `artifact_conflict` | The hash is already stored in the space with a different size; the declared size is wrong. |
| `invalid_artifact` / `invalid_artifact_size` | An upload request needs a lowercase SHA-256, a valid media type, and a size from 0 to `INBOX_OBJECT_STORE_MAX_BYTES`. |
| `invalid_artifact_reference` | An `inbox-artifact` reference must be `inbox-artifact://<space>/<sha256>`, sit in a group message to that same space, and repeat its SHA-256. |
| `artifact_not_available` | The referenced artifact is not available in the message's space with that size. Complete the upload first. |
| `object_store_unavailable` | The inbox could not reach or use the object store. Check the endpoint, credentials, bucket permissions, and `docker compose logs server`. |
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
that machine. Export/import the nine data tables in dependency order:

```sh
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT sequence,id,sender_id,recipient_id,space_id,tenant_id,kind,task_id,thread_id,reply_to,envelope_json,accepted_at,acknowledged_at FROM messages ORDER BY sequence' > messages.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT agent_id,nonce,created_at FROM request_nonces' > request_nonces.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT sequence,occurred_at,action,actor_id,subject_id,tenant_id,outcome,code,detail_json FROM audit_log ORDER BY sequence' > audit_log.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT message_id,attempts,last_notified_at,escalated_at,disposition FROM notification_state' > notification_state.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT message_id,recipient_id,tenant_id,acknowledged_at,disposition,attempts,last_notified_at,escalated_at FROM deliveries' > deliveries.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT id,sha256,agents_json,spaces_json,observed_at FROM registry_state' > registry_state.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT tenant_id,space_id,note_id,head_revision,created_by,created_at FROM notes' > notes.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT tenant_id,space_id,note_id,revision,base_revision,revision_id,author_id,key_id,created_at,media_type,sha256,body,signature,accepted_at FROM note_revisions' > note_revisions.csv
sqlite3 -header -csv agent-inbox-backup.db \
  'SELECT tenant_id,space_id,sha256,size,media_type,state,uploaded_by,created_at,stored_at FROM artifacts' > artifacts.csv
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
\copy messages(sequence,id,sender_id,recipient_id,space_id,tenant_id,kind,task_id,thread_id,reply_to,envelope_json,accepted_at,acknowledged_at) FROM 'messages.csv' CSV HEADER
\copy request_nonces(agent_id,nonce,created_at) FROM 'request_nonces.csv' CSV HEADER
\copy audit_log(sequence,occurred_at,action,actor_id,subject_id,tenant_id,outcome,code,detail_json) FROM 'audit_log.csv' CSV HEADER
\copy notification_state(message_id,attempts,last_notified_at,escalated_at,disposition) FROM 'notification_state.csv' CSV HEADER
\copy deliveries(message_id,recipient_id,tenant_id,acknowledged_at,disposition,attempts,last_notified_at,escalated_at) FROM 'deliveries.csv' CSV HEADER
\copy registry_state(id,sha256,agents_json,spaces_json,observed_at) FROM 'registry_state.csv' CSV HEADER
\copy notes(tenant_id,space_id,note_id,head_revision,created_by,created_at) FROM 'notes.csv' CSV HEADER
\copy note_revisions(tenant_id,space_id,note_id,revision,base_revision,revision_id,author_id,key_id,created_at,media_type,sha256,body,signature,accepted_at) FROM 'note_revisions.csv' CSV HEADER
\copy artifacts(tenant_id,space_id,sha256,size,media_type,state,uploaded_by,created_at,stored_at) FROM 'artifacts.csv' CSV HEADER
SELECT setval(pg_get_serial_sequence('messages','sequence'), COALESCE((SELECT MAX(sequence) FROM messages), 1), EXISTS(SELECT 1 FROM messages));
SELECT setval(pg_get_serial_sequence('audit_log','sequence'), COALESCE((SELECT MAX(sequence) FROM audit_log), 1), EXISTS(SELECT 1 FROM audit_log));
SQL
```

Compare row counts and maximum message/audit sequence values between the
snapshot and PostgreSQL. Run `inboxd audit --limit 100`, verify known agent
polls and acknowledgements, and inspect delivery state before allowing the
proxy to reopen traffic. Keep the SQLite snapshot intact until PostgreSQL
backup and restore have been verified. Never operate both databases as active
inboxes during or after cutover; that would split ordering, replay protection,
deduplication, and audit history.
