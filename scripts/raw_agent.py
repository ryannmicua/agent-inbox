#!/usr/bin/env python3
"""Standalone agent-inbox client written from docs/protocol.md.

It shows that any runtime able to sign Ed25519 can join without the Go CLI.
It needs Python 3.9+ and the `cryptography` package.

Commands (flags fall back to INBOX_SERVER, INBOX_AGENT, INBOX_KEY, INBOX_KEY_ID):
  raw_agent.py vectors FILE
  raw_agent.py poll [--limit N]
  raw_agent.py ack --message ID
  raw_agent.py send --to ID --kind instruction|result --payload JSON
                    [--id ID] [--task ID] [--thread ID] [--reply-to ID]
                    [--artifact URI|TYPE|SHA256|SIZE]...
"""

import argparse
import base64
import datetime
import hashlib
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.request
import uuid

try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
except ImportError:  # pragma: no cover - reported to the operator
    sys.exit("raw_agent.py needs the 'cryptography' package (pip install cryptography)")

BACKSLASH = chr(92)
SHORT_ESCAPES = {'"': '"', BACKSLASH: BACKSLASH, chr(8): "b", chr(12): "f", chr(10): "n", chr(13): "r", chr(9): "t"}
REQUEST_PREFIX = "agent-inbox-request-v1"
ENVELOPE_PREFIX = "agent-inbox-envelope-v1\n"


class RawNumber(str):
    """A JSON number kept as the exact text it was written with."""


def reject_duplicates(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError(f"duplicate JSON object key {key!r}")
        out[key] = value
    return out


def parse_json(text):
    return json.loads(text, parse_int=RawNumber, parse_float=RawNumber, object_pairs_hook=reject_duplicates)


def encode_string(text):
    out = ['"']
    for ch in text:
        code = ord(ch)
        if ch in SHORT_ESCAPES:
            out.append(BACKSLASH + SHORT_ESCAPES[ch])
        elif code < 0x20 or ch in "<>&" or code in (0x2028, 0x2029):
            out.append(BACKSLASH + "u%04x" % code)
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def encode(value):
    """Compact JSON with Go's string escaping and sorted object keys."""
    if isinstance(value, RawNumber):
        return str(value)
    if isinstance(value, bool):
        return "true" if value else "false"
    if value is None:
        return "null"
    if isinstance(value, str):
        return encode_string(value)
    if isinstance(value, int):
        return str(value)
    if isinstance(value, list):
        return "[" + ",".join(encode(item) for item in value) + "]"
    if isinstance(value, dict):
        keys = sorted(value, key=lambda k: k.encode("utf-8"))
        return "{" + ",".join(encode_string(k) + ":" + encode(value[k]) for k in keys) + "}"
    raise TypeError(f"cannot encode {type(value).__name__}")


def canonical_object(env):
    """The canonical envelope object text, members in protocol order."""
    members = [(name, encode_string(env.get(name, ""))) for name in ("type", "id", "task_id", "thread_id")]
    if env.get("reply_to"):
        members.append(("reply_to", encode_string(env["reply_to"])))
    for name in ("sender_id", "recipient_id", "key_id", "kind", "created_at"):
        members.append((name, encode_string(env.get(name, ""))))
    if env.get("asserted_authority"):
        members.append(("asserted_authority", encode_string(env["asserted_authority"])))
    members.append(("content_is_data", encode(bool(env.get("content_is_data")))))
    payload = env.get("payload")
    members.append(("payload", encode({} if payload is None else payload)))
    artifacts = []
    for a in env.get("artifacts") or []:
        artifacts.append("{" + ",".join(f'"{k}":' + encode(a[k]) for k in ("uri", "media_type", "sha256", "size")) + "}")
    members.append(("artifacts", "[" + ",".join(artifacts) + "]"))
    if env.get("provenance"):
        members.append(("provenance", encode(env["provenance"])))
    return "{" + ",".join(encode_string(name) + ":" + text for name, text in members) + "}"


def canonical_envelope(env):
    return (ENVELOPE_PREFIX + canonical_object(env)).encode("utf-8")


def request_bytes(agent, key_id, method, path, timestamp, nonce, body):
    digest = hashlib.sha256(body).hexdigest()
    return "\n".join([REQUEST_PREFIX, agent, key_id, method, path, timestamp, nonce, digest]).encode("utf-8")


def load_key(path):
    raw = base64.b64decode(open(path, encoding="ascii").read().strip())
    if len(raw) != 64:
        sys.exit("key file must hold a base64 64-byte Ed25519 private key")
    return Ed25519PrivateKey.from_private_bytes(raw[:32])  # seed || public key


def b64(data):
    return base64.b64encode(data).decode("ascii")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None  # a signature names one exact path; never follow redirects


OPENER = urllib.request.build_opener(NoRedirect)


def call(args, method, path, body=b""):
    key = load_key(args.key)
    timestamp = str(int(time.time()))
    nonce = base64.urlsafe_b64encode(secrets.token_bytes(24)).decode("ascii").rstrip("=")
    signature = key.sign(request_bytes(args.agent, args.key_id, method, path, timestamp, nonce, body))
    headers = {
        "X-Agent-ID": args.agent,
        "X-Key-ID": args.key_id,
        "X-Request-Timestamp": timestamp,
        "X-Request-Nonce": nonce,
        "X-Request-Signature": b64(signature),
    }
    if body:
        headers["Content-Type"] = "application/json"
    request = urllib.request.Request(args.server.rstrip("/") + path, data=body or None, method=method, headers=headers)
    try:
        with OPENER.open(request, timeout=30) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as err:
        return err.code, err.read()


def finish(status, data):
    text = data.decode("utf-8").strip()
    if 200 <= status < 300:
        print(text)
        return 0
    print(f"raw_agent.py: server returned HTTP {status}: {text}", file=sys.stderr)
    return 1


def cmd_send(args):
    message_id = args.id or str(uuid.uuid4())
    thread = args.thread or message_id
    env = {
        "type": "message",
        "id": message_id,
        "task_id": args.task or thread,
        "thread_id": thread,
        "reply_to": args.reply_to,
        "sender_id": args.agent,
        "recipient_id": args.to,
        "key_id": args.key_id,
        "kind": args.kind,
        "created_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "content_is_data": True,
        "payload": parse_json(args.payload),
        "artifacts": [],
    }
    for value in args.artifact:
        parts = value.split("|")
        if len(parts) != 4:
            sys.exit(f"invalid --artifact {value!r}: expected URI|media-type|sha256|size")
        env["artifacts"].append({"uri": parts[0], "media_type": parts[1], "sha256": parts[2], "size": int(parts[3])})
    canonical = canonical_envelope(env)
    signature = b64(load_key(args.key).sign(canonical))
    # The canonical object plus its signature is one valid wire encoding.
    body = canonical_object(env)[:-1] + ',"signature":' + encode_string(signature) + "}"
    return finish(*call(args, "POST", "/v1/messages", body.encode("utf-8")))


def cmd_vectors(path):
    vectors = parse_json(open(path, encoding="utf-8").read())
    key = Ed25519PrivateKey.from_private_bytes(base64.b64decode(vectors["seed_base64"]))
    public = key.public_key().public_bytes_raw()
    if b64(public) != vectors["public_key_base64"]:
        print("public key does not match the seed", file=sys.stderr)
        return 1
    for v in vectors["envelopes"]:
        canonical = canonical_envelope(v["envelope"])
        if canonical.decode("utf-8") != v["canonical"] or b64(key.sign(canonical)) != v["signature"]:
            print(f"envelope vector {v['name']} differs:\n got {canonical!r}\nwant {v['canonical']!r}", file=sys.stderr)
            return 1
    for r in vectors["requests"]:
        canonical = request_bytes(r["agent_id"], r["key_id"], r["method"], r["path"], r["timestamp"], r["nonce"], r["body"].encode("utf-8"))
        if canonical.decode("utf-8") != r["canonical"] or b64(key.sign(canonical)) != r["signature"]:
            print(f"request vector {r['name']} differs", file=sys.stderr)
            return 1
    print(f"ok: {len(vectors['envelopes'])} envelope and {len(vectors['requests'])} request vectors match")
    return 0


def main(argv):
    parser = argparse.ArgumentParser(description="Standalone agent-inbox client (see docs/protocol.md).")
    sub = parser.add_subparsers(dest="command", required=True)
    vec = sub.add_parser("vectors", help="check this client against the published signing vectors")
    vec.add_argument("file")
    for name in ("poll", "ack", "send", "spaces"):
        p = sub.add_parser(name)
        p.add_argument("--server", default=os.environ.get("INBOX_SERVER"))
        p.add_argument("--agent", default=os.environ.get("INBOX_AGENT"))
        p.add_argument("--key", default=os.environ.get("INBOX_KEY"))
        p.add_argument("--key-id", default=os.environ.get("INBOX_KEY_ID", "primary"))
        if name == "poll":
            p.add_argument("--limit", type=int, default=100)
        elif name == "ack":
            p.add_argument("--message", required=True)
        elif name == "send":
            p.add_argument("--to", required=True)
            p.add_argument("--kind", required=True)
            p.add_argument("--payload", default="{}")
            p.add_argument("--id")
            p.add_argument("--task")
            p.add_argument("--thread")
            p.add_argument("--reply-to", default="")
            p.add_argument("--artifact", action="append", default=[])
    args = parser.parse_args(argv)
    if args.command == "vectors":
        return cmd_vectors(args.file)
    if not (args.server and args.agent and args.key):
        parser.error("--server, --agent and --key (or INBOX_SERVER, INBOX_AGENT, INBOX_KEY) are required")
    if args.command == "poll":
        return finish(*call(args, "GET", f"/v1/messages?limit={args.limit}"))
    if args.command == "ack":
        return finish(*call(args, "POST", f"/v1/messages/{args.message}/ack", b'{"processed":true}'))
    if args.command == "spaces":
        return finish(*call(args, "GET", "/v1/spaces"))
    return cmd_send(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
