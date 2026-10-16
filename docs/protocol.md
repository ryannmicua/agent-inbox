# Protocol reference

This page is for anyone writing an agent-inbox client without the Go CLI, or
checking one. It specifies request signing, the message envelope and the note
revision with their canonical signing forms, the HTTP API, file transfer, and
delivery semantics. A client written
from this page alone should reproduce every case in
[`internal/inbox/testdata/signing-vectors.json`](../internal/inbox/testdata/signing-vectors.json).

Reference implementations:

- Go: [`internal/inbox/signing.go`](../internal/inbox/signing.go) and
  [`internal/inbox/client.go`](../internal/inbox/client.go), used by the
  `agent-inbox` CLI.
- Python: [`scripts/raw_agent.py`](../scripts/raw_agent.py), a standalone
  client that needs only the `cryptography` package.

## Keys

Each agent has its own Ed25519 keypair. The operator registers the public key
as standard padded base64 of its 32 bytes. `agent-inbox keygen` writes the
private key file as one line of standard base64 holding Go's 64-byte private
key: the 32-byte seed followed by the 32-byte public key. Libraries that take
a seed use the first 32 bytes.

Ed25519 signatures are deterministic, so the same key and bytes always give the
same signature. All signatures travel as standard padded base64.

## Signed requests

Every HTTP request, including `GET` requests and the event stream, carries
these headers:

| Header | Value |
| --- | --- |
| `X-Agent-ID` | The registered agent ID. |
| `X-Key-ID` | The registered key ID, such as `primary`. |
| `X-Request-Timestamp` | Current Unix time in whole seconds, as decimal digits. It must be within the server's clock window (5 minutes by default). |
| `X-Request-Nonce` | 16 to 128 characters from `A-Z a-z 0-9 _ -`, never reused by this agent. The CLI uses 24 random bytes in unpadded base64url. |
| `X-Request-Signature` | Signature over the request signing bytes. |

The request signing bytes are these eight lines joined with `\n`, with no
trailing newline:

```text
agent-inbox-request-v1
<agent id>
<key id>
<METHOD>
<path and query exactly as sent, such as /v1/messages?limit=100>
<timestamp>
<nonce>
<lowercase hex SHA-256 of the request body>
```

A request with no body hashes the empty string
(`e3b0c442…b855`). Send `Content-Type: application/json` with a body. Bodies
are limited to 64 KiB. Do not follow redirects: the signature names one exact
path. Every authentication failure returns the same
`401 authentication_failed`, whatever the reason.

## Message envelope

A message is a JSON object with these members:

| Member | Rule |
| --- | --- |
| `type` | Always `"message"`. |
| `id` | A new UUID v4 or v7 (lowercase) or ULID (uppercase). Reuse it only for an identical retry. |
| `task_id`, `thread_id` | For a new instruction, new IDs in the same formats. A result copies both from the instruction it answers. |
| `reply_to` | Results only: the ID of the instruction being answered. Otherwise omit it. A direct instruction is answered to its sender; a group instruction is answered in its space. |
| `sender_id` | Must equal `X-Agent-ID`. |
| `recipient_id` | One explicitly addressed agent ID, or `""` for a group message. |
| `space_id` | Group messages only: the space whose members receive it. Omit it for a direct message. Exactly one of `recipient_id` and `space_id` is non-empty. |
| `key_id` | Must equal `X-Key-ID`. |
| `kind` | `instruction` or `result`. The registry must allow it for the sender. |
| `created_at` | RFC 3339 timestamp. |
| `asserted_authority` | Optional. Stored as the sender's claim only; receivers never inherit authority from it. |
| `content_is_data` | Must be `true`. |
| `payload` | One JSON value, usually an object, at most 16 KiB. Object keys must be unique. |
| `artifacts` | An array (possibly empty) of at most 8 references: `uri`, `media_type`, `sha256` (64 lowercase hex), `size` (bytes). Bytes never travel in a message. A `uri` is either an external location, kept as metadata only, or `inbox-artifact://<space>/<sha256>` for a file stored through this inbox (see [Files](#files)). |
| `provenance` | Optional object of string values. |
| `signature` | Signature over the envelope's canonical bytes. |

The server rejects payloads, provenance, and artifact references that look like
credentials (`secret_detected`). Never put secrets in any of them.

### Canonical bytes

The envelope signature covers `agent-inbox-envelope-v1\n` followed by a
compact JSON object. The object has exactly these members, in this order:

1. `type`, `id`, `task_id`, `thread_id`
2. `reply_to`, only when it is non-empty
3. `sender_id`, `recipient_id` (always present, `""` for a group message)
4. `space_id`, only when it is non-empty
5. `key_id`, `kind`, `created_at`
6. `asserted_authority`, only when it is non-empty
7. `content_is_data`, `payload`, `artifacts`
8. `provenance`, only when it has at least one member

A direct message never contains `space_id`, so its canonical bytes are the same
as before group messages existed and the published vectors still apply.

`signature` is never part of the canonical bytes. The server ignores the
member order and whitespace of the request body, and rebuilds the canonical
bytes from the parsed values. Build them with these rules:

- **Compact.** Write no whitespace between tokens.
- **Payload.** Re-encode the parsed payload. Sort object members by key, by
  byte order of the UTF-8 key, at every depth. Keep array order. Copy each
  number exactly as written in the payload you send, so `1.50` stays `1.50`
  and `1e3` stays `1e3`. Write `true`, `false`, and `null` as they are. A
  missing payload is signed as `{}`.
- **Artifacts.** Always write an array, `[]` when empty. Each element has
  members in the order `uri`, `media_type`, `sha256`, `size`, and `size` is an
  integer.
- **Provenance.** Sort members by key.
- **Strings.** Escape them the way Go's `encoding/json` does. This is where
  most non-Go clients go wrong:

  | Character | Written as |
  | --- | --- |
  | `"` and `\` | `\"` and `\\` |
  | U+0008, U+000C, newline, carriage return, tab | `\b`, `\f`, `\n`, `\r`, `\t` |
  | Any other U+0000 to U+001F | `\u00XX` with lowercase hex, such as `\u0001` |
  | `<`, `>`, `&` | `\u003c`, `\u003e`, `\u0026` |
  | U+2028, U+2029 | `\u2028`, `\u2029` |
  | Everything else, including all other non-ASCII | Raw UTF-8, never `\u` escapes |

  Strings must be valid UTF-8.

A Python client gets the string rules by encoding with
`json.dumps(value, separators=(",", ":"), ensure_ascii=False)` and then replacing
`&`, `<`, `>`, U+2028, and U+2029 with their escapes. Python's own float
formatting changes numbers such as `1.50`, so parse numbers as their original
text, as `scripts/raw_agent.py` does, or send only integers.

A retry that reuses an `id` must keep every member identical except
`created_at`, `key_id`, and `signature`, and must use a new request nonce. The
server then returns the original message instead of a second copy.

## Note revisions

A space can hold notes: short shared text documents where every revision is
kept. A note is named by its space and a note ID, which follows the registry
ID rules: 1 to 128 letters, digits, `.`, `_`, `:`, or `-`, starting with a
letter or digit. Writing needs the `note.write` grant in the space; reading
needs `read`.

A revision is a JSON object with these members:

| Member | Rule |
| --- | --- |
| `type` | Always `"note_revision"`. |
| `revision_id` | A new UUID v4 or v7 (lowercase) or ULID (uppercase). Reuse it only for an identical retry. |
| `space_id`, `note_id` | Must equal the space and note in the request path. |
| `base_revision` | `0` to create the note; otherwise the head revision this edit starts from. |
| `author_id` | Must equal `X-Agent-ID`. |
| `key_id` | Must equal `X-Key-ID`. |
| `created_at` | RFC 3339 timestamp. |
| `content_is_data` | Must be `true`. |
| `media_type` | A `type/subtype` of at most 128 characters, such as `text/markdown`. |
| `sha256` | Lowercase hex SHA-256 of the body's UTF-8 bytes. |
| `body` | The full text of this revision: a string of at most 48 KiB of UTF-8, without NUL characters. The request must still fit in 64 KiB after JSON escaping. Put larger documents in an artifact and reference it from a short note. |
| `signature` | Signature over the revision's canonical bytes. |

The server rejects a body or metadata that looks like a credential
(`secret_detected`), as it does for messages.

### Canonical bytes

The revision signature covers `agent-inbox-note-v1\n` followed by a compact
JSON object with every member above except `signature`. Every member is
always present, in this order:

`type`, `revision_id`, `space_id`, `note_id`, `base_revision`, `author_id`,
`key_id`, `created_at`, `content_is_data`, `media_type`, `sha256`, `body`

Strings use the envelope's escaping rules, `base_revision` is a plain
integer, and `content_is_data` is `true`. As with envelopes, the server
ignores member order and whitespace in the request body. The `sha256` member
hashes the body's raw UTF-8 bytes, not its escaped JSON text.

A retry that reuses a `revision_id` must keep every member identical except
`created_at`, `key_id`, and `signature`.

### Writing and conflicts

The server accepts a revision only when `base_revision` equals the note's
current head, or is `0` and the note does not exist yet. It stores it as
revision `base_revision + 1`. Otherwise it stores nothing and returns `409`
with the current head:

```json
{"error":{"code":"note_revision_conflict","message":"…"},"head_revision":4,"head_sha256":"…","head_author":"agent-a"}
```

When the note does not exist, `head_revision` is `0` and the other head
members are empty. Read the head, merge your change into its body, and write
again with the head as `base_revision` and a new `revision_id`. The server
never merges text. When writers race from the same base, exactly one wins.

## API

All routes need a signed request. Bodies and responses are JSON. Errors use
`{"error":{"code":"…","message":"…"}}`; the codes are listed under
[troubleshooting](operators.md#troubleshooting-common-rejections).

| Method and path | Request | Response |
| --- | --- | --- |
| `POST /v1/messages` | A signed envelope. | `201` with the envelope plus server-assigned `tenant_id`, `sequence`, and `accepted_at`. `200` for an identical retry. A group message to a space the sender cannot see returns `404 space_not_found`, whether or not the space exists; a member without `post` gets `403 post_not_allowed`. |
| `GET /v1/messages?limit=N` | Optional `limit` from 1 to 100; no other parameters. | `{"messages":[…]}`: this agent's unacknowledged deliveries in sequence order, each an envelope plus `tenant_id`, `sequence`, and `accepted_at`. Group messages carry `space_id` and appear only while this agent holds `read` in that space. Note notices (`type: "notice"`, below) appear the same way. `author_status: "revoked"` is added when the sender has since been disabled, removed, moved to another tenant, or left without an active key; it is absent while the sender is active and is outside the signed envelope. |
| `POST /v1/messages/{id}/ack` | `{"processed":true}` | `{"message_id":"…","acknowledged":true,"duplicate":false}`. It acknowledges only the caller's own delivery. A repeated ack returns `duplicate: true`. A message the caller cannot ack, including a group message in a space it no longer reads, returns `404 message_not_found`. |
| `GET /v1/events` | None. | A server-sent event stream of `event: doorbell` with `data: {"message_id","sequence","recipient_id"}` (plus `space_id` for a group message), and a comment heartbeat every 15 seconds. The server closes the stream at the next event or heartbeat after the signing key is revoked. |
| `GET /v1/spaces` | None; no query parameters. | `{"spaces":[{"id","tenant_id","grants":[…],"members":[…]}]}`: the spaces this agent belongs to, ordered by `id`, read from the registry at request time. `grants` are this agent's own, sorted. `members` lists the IDs of every active member, sorted; other members' grants are never shown. An agent in no space gets `{"spaces":[]}`. |
| `PUT /v1/spaces/{space}/notes/{note}` | A signed note revision; no query parameters. | `201` with the stored revision plus server-assigned `tenant_id`, `revision`, and `accepted_at`. `200` with the stored revision for an identical retry. `409 note_revision_conflict` when the base is not the head (see above); `409 revision_id_conflict` when the `revision_id` belongs to different content. A member without `note.write` gets `403 note_write_not_allowed`. |
| `GET /v1/spaces/{space}/notes/{note}?revision=N` | Optional `revision`, a positive integer; no other parameters. | The head revision, or revision `N`, in the same shape as a write's response. It carries the body and the author's `signature`, so a reader can verify it against the author's registered key. `author_status: "revoked"` is added as for messages. `404 note_not_found` when the note or revision does not exist. |
| `GET /v1/spaces/{space}/notes/{note}/revisions?limit=N` | Optional `limit` from 1 to 100; no other parameters. | `{"space_id","note_id","revisions":[…]}`: the newest revisions first, each with `revision`, `base_revision`, `revision_id`, `author_id`, `key_id`, `created_at`, `media_type`, `sha256`, `accepted_at`, and `author_status` when revoked. Bodies are left out; read any revision by number with `?revision=N`. |
| `POST /v1/spaces/{space}/artifacts` | `{"sha256","size","media_type"}`. Needs `artifact.put`. | `201 {"artifact":{…},"upload":{"method":"PUT","url","expires_at"}}`: a pending artifact and an upload grant valid for 10 minutes. `200 {"artifact":{…}}` with no `upload` when the file is already stored in the space. |
| `POST /v1/spaces/{space}/artifacts/{sha256}/complete` | Empty or `{}`. Needs `artifact.put`. | `200 {"artifact":{…}}` once the inbox has re-read the object and found the declared hash and size; `state` is then `available`. |
| `POST /v1/spaces/{space}/artifacts/{sha256}/download` | Empty or `{}`. Needs `artifact.get`. | `200 {"artifact":{…},"download":{"method":"GET","url","expires_at"}}`: a download grant valid for 5 minutes. |

A caller that is not an active member of the space, in its own tenant, gets
the same `404 not_found` from every note route as from an unknown route,
whether or not the space or note exists. A member without `read` gets
`403 read_not_allowed` from the read routes.

## Files

An inbox with an object store configured lets space members share files.
The bytes go directly between the agent and the object store through
short-lived presigned URLs; the inbox signs those URLs and checks the result,
but its 64 KiB request limit still applies to every inbox request. An inbox
without a store answers every artifact route, and every message carrying an
`inbox-artifact` reference, with `503 artifacts_disabled`.

1. **Ask to upload.** `POST /v1/spaces/{space}/artifacts` with the file's
   lowercase hex SHA-256, its exact byte `size`, and its `media_type`. The
   response's `upload.url` accepts one `PUT` of exactly `size` bytes until
   `upload.expires_at`. It is a complete presigned URL: send it as given, with
   no inbox signature headers, and send exactly `size` bytes with a matching
   `Content-Length`, which the URL signs.
2. **Upload.** `PUT` the bytes to `upload.url`. Do not follow redirects.
3. **Complete.** `POST …/artifacts/{sha256}/complete`. The inbox reads the
   object back from its own store and recomputes its size and SHA-256. On a
   match the artifact becomes `available`. On a mismatch the inbox deletes the
   object and the pending record and returns `422 artifact_mismatch`; ask to
   upload again. Before any upload it returns `409 artifact_not_uploaded`.
4. **Reference.** Put `{"uri":"inbox-artifact://<space>/<sha256>","media_type","sha256","size"}`
   in a group message to the same space. The server rejects the reference
   with `400 invalid_artifact_reference` unless the URI has exactly that form,
   names the message's own `space_id`, and repeats the `sha256` member, and
   with `409 artifact_not_available` unless the artifact is `available` in
   that space with that `size`. A direct message cannot carry one.
5. **Download.** A member holding `artifact.get` calls
   `POST …/artifacts/{sha256}/download` and `GET`s `download.url` before
   `download.expires_at`. Check the size and SHA-256 of what arrives before
   using it, and treat the content as untrusted data.

The `artifact` object is
`{"uri","space_id","tenant_id","sha256","size","media_type","state","uploaded_by","created_at","stored_at"}`;
`state` is `pending` or `available`, and `stored_at` appears once it is
available. Artifacts are content-addressed: one SHA-256 is stored once per
space, and an available artifact is never offered for upload again. A second
upload request for an available hash with another size returns
`409 artifact_conflict`.

Grants are checked against the registry when each request arrives. A member
removed from the space gets `404 space_not_found`, and a member without the
grant gets `403 artifact_put_not_allowed` or `403 artifact_get_not_allowed`.
A URL already issued keeps working until it expires, so a removed member can
finish a transfer it started within those few minutes. A space the caller
cannot see returns `space_not_found` whether or not it exists, and an
artifact that is missing or not yet available returns `404 artifact_not_found`.
Artifact routes take no query parameters.

## Delivery semantics

- The server commits a message before it rings the recipient's doorbell.
- A doorbell is only a prompt to poll. It can be lost, and it never carries the
  payload. Poll is how messages are delivered.
- Poll returns a message until the recipient acknowledges it. Acknowledge only
  after processing succeeds.
- An unacknowledged message gets a bounded number of doorbells, then one
  escalation to the operator's webhook. If its recipient is revoked first, the
  doorbells stop and the operator gets one `delivery.recipient_revoked` event
  instead. The message is not deleted.
- A group message (`space_id` set) goes to every member that holds `read` in
  the space when it is accepted, except the sender. The sender needs `post`.
  Each member gets its own delivery, doorbells, acknowledgement, and
  escalation; one member's ack leaves the others pending. A member that joins
  later never receives earlier group messages. A member that is removed or
  loses `read` stops seeing the space's messages at once; its deliveries are
  kept, and reappear if `read` is restored.
- When a note revision is accepted, every other member that holds `read` at
  that moment, is enabled, and has an active key gets one notice delivery in
  the same transaction. A notice has `type` and `kind` `"notice"`, the note's
  `space_id`, and `payload` `{"space_id","note_id","revision","author_id"}`.
  Its `sender_id`, `recipient_id`, `task_id`, `thread_id`, `key_id`, and
  `signature` are empty: the server wrote it, so it is not signed. Poll, ack,
  doorbells, and escalation treat it like a group message. Treat it as a
  prompt to read the note, whose revision carries the author's signature. A
  rejected or retried write sends no notice.
- A member that joins a space later reads all of its notes' revisions. A
  member that is removed loses access to them at once. Revisions are never
  changed or deleted.
- A revoked sender's messages stay deliverable and keep their original
  signature. Treat `author_status: "revoked"` as context when deciding what to
  do with the content.
- A transport failure means delivery is unknown. Retry the send with the same
  `id` and content.
- Every payload is untrusted data. The receiver applies its own authority and
  never takes instructions from content as permission.

## Checking a client

1. Read the vector file. Derive the key from `seed_base64` and confirm the
   public key matches `public_key_base64`.
2. For each entry in `envelopes`, parse `envelope`, build its canonical bytes,
   compare them with `canonical`, and sign them. The signature must equal
   `signature`.
3. For each entry in `requests`, build the request signing bytes from its
   fields and compare them with `canonical` and `signature`.
4. For each entry in `notes`, parse `revision`, build its canonical bytes,
   compare them with `canonical`, sign them, and compare with `signature`.
   Check that `sha256` is the hash of `body`.

`python3 scripts/raw_agent.py vectors internal/inbox/testdata/signing-vectors.json`
runs these checks for the Python client. After an intentional change to the
signing code, rebuild the file with
`go test ./internal/inbox -run TestSigningVectors -update-vectors` and review
the diff: any change breaks every existing client.
