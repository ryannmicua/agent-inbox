#!/usr/bin/env bash
# Replay the two-runtime walkthrough: a bot using the Go CLI and a bot using
# the standalone Python client exchange an instruction with a file reference,
# poll and acknowledge while offline, reply with a correlated result, list
# the spaces the operator granted them, and exchange a group message in a
# space, each acknowledging only its own delivery.
#
#   scripts/walkthrough.sh          local: build, start inboxd on loopback,
#                                   replay, edit the Python bot's space grants
#                                   and remove it from the space, then revoke
#                                   it and show what the audit records
#   scripts/walkthrough.sh remote   replay against an existing server; set
#                                   INBOX_SERVER, CLI_BOT, CLI_KEY, RAW_BOT, RAW_KEY,
#                                   and optionally WALKTHROUGH_SPACE: both bots
#                                   must then hold read and post there, and
#                                   they exchange a group message in it
#
# Needs Go, Python 3, and the Python 'cryptography' package. Remote mode can
# skip Go by setting AGENT_INBOX_BIN to a prebuilt agent-inbox CLI. In remote mode the
# operator must have registered both bots, each allowed to message the other
# with kinds instruction and result. Revocation is an operator registry edit,
# so only local mode replays it. Both bots should start with empty inboxes.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"
mode="${1:-local}"
if [[ "$mode" != local && "$mode" != remote ]]; then
  echo "usage: $0 [local|remote]" >&2
  exit 2
fi
if [[ "$mode" == local || -z "${AGENT_INBOX_BIN:-}" ]]; then
  command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
fi
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }
python3 -c 'import cryptography' 2>/dev/null || { echo "the Python 'cryptography' package is required" >&2; exit 1; }

tmp_dir="$(mktemp -d)"
pids=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
json_field() { python3 -c 'import json,sys; v=json.load(sys.stdin)
for k in sys.argv[1].split("."): v=v[int(k)] if k.isdigit() else v[k]
print(v)' "$1"; }

if [[ "$mode" == remote && -n "${AGENT_INBOX_BIN:-}" ]]; then
  cli_bin="$AGENT_INBOX_BIN"
else
  go build -trimpath -o "$tmp_dir/agent-inbox" ./cmd/agent-inbox
  cli_bin="$tmp_dir/agent-inbox"
fi
raw_agent=(python3 "$repo_root/scripts/raw_agent.py")

if [[ "$mode" == local ]]; then
  go build -trimpath -o "$tmp_dir/inboxd" ./cmd/inboxd
  free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'; }
  server_port="$(free_port)"
  sink_port="$(free_port)"
  CLI_BOT=cli-bot RAW_BOT=raw-bot
  CLI_KEY="$tmp_dir/cli-bot.key" RAW_KEY="$tmp_dir/raw-bot.key"
  "$cli_bin" keygen --private-key "$CLI_KEY" --public-key "$tmp_dir/cli-bot.pub" >/dev/null
  "$cli_bin" keygen --private-key "$RAW_KEY" --public-key "$tmp_dir/raw-bot.pub" >/dev/null
  # write_registry RAW_DISABLED RAW_GRANTS: an empty RAW_GRANTS removes
  # raw-bot from the release-notes space.
  write_registry() {
    local raw_disabled="$1" raw_member=""
    [[ -n "$2" ]] && raw_member=",{\"agent\":\"raw-bot\",\"grants\":$2}"
    cat > "$tmp_dir/registry.json.tmp" <<EOF
{"version":2,"agents":[
 {"id":"cli-bot","tenant_id":"walkthrough","public_keys":[{"id":"primary","public_key":"$(tr -d '\r\n' < "$tmp_dir/cli-bot.pub")"}],"allowed_recipients":["raw-bot"],"allowed_kinds":["instruction","result"]},
 {"id":"raw-bot","tenant_id":"walkthrough","disabled":$raw_disabled,"public_keys":[{"id":"primary","public_key":"$(tr -d '\r\n' < "$tmp_dir/raw-bot.pub")"}],"allowed_recipients":["cli-bot"],"allowed_kinds":["instruction","result"]}
],"spaces":[
 {"id":"release-notes","tenant_id":"walkthrough","members":[
  {"agent":"cli-bot","grants":["read","post","note.write","artifact.put","artifact.get"]}$raw_member]}
]}
EOF
    mv "$tmp_dir/registry.json.tmp" "$tmp_dir/registry.json"
  }
  write_registry false '["read","post","note.write","artifact.get"]'
  WEBHOOK_SINK_HOST=127.0.0.1 WEBHOOK_SINK_PORT="$sink_port" python3 scripts/webhook_sink.py > "$tmp_dir/notifications.log" 2>&1 &
  pids+=("$!")
  export INBOX_LISTEN_ADDR="127.0.0.1:$server_port" INBOX_DB_PATH="$tmp_dir/inbox.db"
  INBOX_REGISTRY_PATH="$tmp_dir/registry.json" \
    INBOX_WEBHOOK_URL="http://127.0.0.1:$sink_port/notifications" INBOX_RETRY_INTERVAL=2s \
    "$tmp_dir/inboxd" > "$tmp_dir/server.log" 2>&1 &
  pids+=("$!")
  INBOX_SERVER="http://127.0.0.1:$server_port"
  for _ in $(seq 1 50); do
    "$tmp_dir/inboxd" healthcheck >/dev/null 2>&1 && break
    sleep 0.2
  done
  "$tmp_dir/inboxd" healthcheck >/dev/null || { cat "$tmp_dir/server.log" >&2; echo "inboxd did not start" >&2; exit 1; }
else
  for name in INBOX_SERVER CLI_BOT CLI_KEY RAW_BOT RAW_KEY; do
    [[ -n "${!name:-}" ]] || { echo "remote mode needs $name" >&2; exit 2; }
  done
fi

cli() { local command="$1"; shift; "$cli_bin" "$command" --server "$INBOX_SERVER" --agent "$CLI_BOT" --key "$CLI_KEY" "$@"; }
raw() { local command="$1"; shift; "${raw_agent[@]}" "$command" --server "$INBOX_SERVER" --agent "$RAW_BOT" --key "$RAW_KEY" "$@"; }

step "Python client matches the published signing vectors"
"${raw_agent[@]}" vectors internal/inbox/testdata/signing-vectors.json

step "$CLI_BOT (Go CLI) describes a file and sends an instruction while $RAW_BOT is offline"
printf 'draft release notes\n' > "$tmp_dir/draft.md"
ref="$("$cli_bin" artifact-ref --file "$tmp_dir/draft.md" --uri "https://files.example.com/draft.md" --media-type text/markdown)"
echo "artifact: $ref"
instruction="$(cli send --to "$RAW_BOT" --kind instruction --payload '{"task":"review <draft> & reply"}' --artifact "$ref")"
instruction_id="$(json_field id <<< "$instruction")"
task_id="$(json_field task_id <<< "$instruction")"
thread_id="$(json_field thread_id <<< "$instruction")"
echo "accepted instruction $instruction_id at sequence $(json_field sequence <<< "$instruction")"

step "$RAW_BOT (Python) comes online and polls"
polled="$(raw poll)"
[[ "$(json_field messages.0.id <<< "$polled")" == "$instruction_id" ]] || { echo "unexpected poll: $polled" >&2; exit 1; }
echo "received $instruction_id with artifact sha256 $(json_field messages.0.artifacts.0.sha256 <<< "$polled")"

step "$RAW_BOT acknowledges after processing; a repeat ack is a duplicate"
raw ack --message "$instruction_id"
raw ack --message "$instruction_id"
[[ "$(raw poll)" == '{"messages":[]}' ]] || { echo "acknowledged message was delivered again" >&2; exit 1; }

step "$RAW_BOT replies with a correlated result"
result="$(raw send --to "$CLI_BOT" --kind result --task "$task_id" --thread "$thread_id" --reply-to "$instruction_id" --payload '{"summary":"looks good < ship it & tag","price":1.50}')"
result_id="$(json_field id <<< "$result")"
echo "accepted result $result_id"

step "$CLI_BOT waits for its inbox, then acknowledges"
returned="$(cli poll --wait 30s)"
[[ "$(json_field messages.0.reply_to <<< "$returned")" == "$instruction_id" ]] || { echo "unexpected poll: $returned" >&2; exit 1; }
echo "received result $(json_field messages.0.id <<< "$returned")"
cli ack --message "$result_id"

step "Both bots list their spaces; each sees only its own grants"
cli_spaces="$(cli spaces)"
raw_spaces="$(raw spaces)"
space_grants() { python3 -c 'import json,sys
space = next((s for s in json.load(sys.stdin)["spaces"] if s["id"] == sys.argv[1]), None)
print("not a member" if space is None else ",".join(space["grants"]) + " (members " + ",".join(space["members"]) + ")")' "$1"; }
if [[ "$mode" == local ]]; then
  WALKTHROUGH_SPACE=release-notes
  [[ "$(space_grants release-notes <<< "$cli_spaces")" == "artifact.get,artifact.put,note.write,post,read (members cli-bot,raw-bot)" ]] || { echo "unexpected spaces for $CLI_BOT: $cli_spaces" >&2; exit 1; }
  [[ "$(space_grants release-notes <<< "$raw_spaces")" == "artifact.get,note.write,post,read (members cli-bot,raw-bot)" ]] || { echo "unexpected spaces for $RAW_BOT: $raw_spaces" >&2; exit 1; }
fi
if [[ -n "${WALKTHROUGH_SPACE:-}" ]]; then
  echo "$CLI_BOT in $WALKTHROUGH_SPACE: $(space_grants "$WALKTHROUGH_SPACE" <<< "$cli_spaces")"
  echo "$RAW_BOT in $WALKTHROUGH_SPACE: $(space_grants "$WALKTHROUGH_SPACE" <<< "$raw_spaces")"
  for listed in "$cli_spaces" "$raw_spaces"; do
    [[ "$(space_grants "$WALKTHROUGH_SPACE" <<< "$listed")" != "not a member" ]] || { echo "a bot is not a member of $WALKTHROUGH_SPACE" >&2; exit 1; }
  done
else
  echo "$CLI_BOT: $cli_spaces"
  echo "$RAW_BOT: $raw_spaces"
fi

if [[ -n "${WALKTHROUGH_SPACE:-}" ]]; then
  step "$CLI_BOT posts one group message to $WALKTHROUGH_SPACE; each member gets its own delivery"
  group="$(cli send --space "$WALKTHROUGH_SPACE" --kind instruction --payload '{"ask":"post your section of the release plan"}')"
  group_id="$(json_field id <<< "$group")"
  echo "accepted group instruction $group_id in space $(json_field space_id <<< "$group")"
  polled="$(raw poll)"
  [[ "$(json_field messages.0.id <<< "$polled")" == "$group_id" && "$(json_field messages.0.space_id <<< "$polled")" == "$WALKTHROUGH_SPACE" ]] || { echo "unexpected poll: $polled" >&2; exit 1; }
  echo "$RAW_BOT received $group_id from space $WALKTHROUGH_SPACE"
  [[ "$(cli poll)" == '{"messages":[]}' ]] || { echo "the sender received its own group message" >&2; exit 1; }
  echo "$CLI_BOT, the sender, holds no delivery of its own message"
  raw ack --message "$group_id"
  [[ "$(raw poll)" == '{"messages":[]}' ]] || { echo "acknowledged group message was delivered again" >&2; exit 1; }
  if [[ "$mode" == local ]]; then
    "$tmp_dir/inboxd" audit --limit 100 | python3 -c 'import json,sys
row = next(e for e in json.load(sys.stdin) if e["action"] == "send" and e["subject_id"] == sys.argv[1] and e["outcome"] == "accepted")
d = row["detail"]
assert d["space_id"] == "release-notes" and d["recipients"] == ["raw-bot"] and len(d["registry_sha256"]) == 64, d
print("audit: send accepted, audience %s at registry %s" % (d["recipients"], d["registry_sha256"][:12]))' "$group_id"
  fi

  step "$RAW_BOT answers in the space with a correlated result"
  group_result="$(raw send --space "$WALKTHROUGH_SPACE" --kind result --task "$(json_field task_id <<< "$group")" --thread "$(json_field thread_id <<< "$group")" --reply-to "$group_id" --payload '{"section":"notes drafted"}')"
  group_result_id="$(json_field id <<< "$group_result")"
  returned="$(cli poll --wait 30s)"
  [[ "$(json_field messages.0.id <<< "$returned")" == "$group_result_id" && "$(json_field messages.0.reply_to <<< "$returned")" == "$group_id" ]] || { echo "unexpected poll: $returned" >&2; exit 1; }
  echo "$CLI_BOT received result $group_result_id in space $(json_field messages.0.space_id <<< "$returned")"
  cli ack --message "$group_result_id"
fi

if [[ "$mode" == local ]]; then
  audit_rows() { "$tmp_dir/inboxd" audit --limit 100; }
  audit_has() { audit_rows | python3 -c 'import json,sys
action, outcome, code = sys.argv[1:4]
sys.exit(0 if any(e["action"] == action and e["outcome"] == outcome and e.get("code","") == code for e in json.load(sys.stdin)) else 1)' "$@"; }

  step "The operator narrows $RAW_BOT's grants, then removes it from the space; no restart"
  write_registry false '["read"]'
  [[ "$(raw spaces | space_grants release-notes)" == "read (members cli-bot,raw-bot)" ]] || { echo "grant edit did not apply" >&2; exit 1; }
  echo "$RAW_BOT in release-notes: $(raw spaces | space_grants release-notes)"
  later="$(cli send --space release-notes --kind instruction --payload '{"ask":"one more section"}')"
  later_id="$(json_field id <<< "$later")"
  [[ "$(json_field messages.0.id <<< "$(raw poll)")" == "$later_id" ]] || { echo "read-only member did not receive the group message" >&2; exit 1; }
  echo "$RAW_BOT, holding read only, received group message $later_id"
  write_registry false ''
  [[ "$(raw spaces)" == '{"spaces":[]}' ]] || { echo "removed member still lists the space" >&2; exit 1; }
  [[ "$(raw poll)" == '{"messages":[]}' ]] || { echo "removed member still sees the space's messages" >&2; exit 1; }
  echo "$RAW_BOT, removed from release-notes, no longer sees $later_id"
  echo "$RAW_BOT in release-notes: $(raw spaces | space_grants release-notes)"
  echo "$CLI_BOT in release-notes: $(cli spaces | space_grants release-notes)"
  audit_rows | python3 -c 'import json,sys
changes = [c for e in json.load(sys.stdin) if e["action"] == "registry.changed" for c in (e.get("detail") or {}).get("memberships_changed", [])]
want = [{"space": "release-notes", "agent": "raw-bot", "old_grants": ["read"], "new_grants": []},
        {"space": "release-notes", "agent": "raw-bot", "old_grants": ["artifact.get", "note.write", "post", "read"], "new_grants": ["read"]}]
sys.exit(0 if changes[:2] == want else "registry.changed did not record the membership edits: %s" % changes)'
  echo "registry.changed recorded both membership edits"
  for _ in $(seq 1 30); do
    audit_has delivery.recipient_revoked attempted not_member && break
    sleep 0.5
  done
  audit_has delivery.recipient_revoked attempted not_member || { echo "the removed member's delivery was not dispositioned" >&2; exit 1; }
  echo "audit: delivery.recipient_revoked not_member for $later_id; the delivery row is kept"

  step "Work is left pending in both directions"
  pending="$(cli send --to "$RAW_BOT" --kind instruction --payload '{"task":"never collected"}')"
  pending_id="$(json_field id <<< "$pending")"
  authored="$(raw send --to "$CLI_BOT" --kind instruction --payload '{"task":"written before revocation"}')"
  authored_id="$(json_field id <<< "$authored")"
  echo "pending for $RAW_BOT: $pending_id; pending for $CLI_BOT: $authored_id"

  step "The operator disables $RAW_BOT (disable first, remove later)"
  write_registry true ''
  if raw poll 2>"$tmp_dir/refused.txt"; then echo "revoked bot could still poll" >&2; exit 1; fi
  cat "$tmp_dir/refused.txt"
  if cli send --to "$RAW_BOT" --kind instruction --payload '{"task":"anything"}' 2>"$tmp_dir/refused.txt"; then
    echo "send to a revoked bot was accepted" >&2; exit 1
  fi
  cat "$tmp_dir/refused.txt"

  step "$CLI_BOT still receives $RAW_BOT's earlier message, flagged as from a revoked author"
  flagged="$(cli poll)"
  [[ "$(json_field messages.0.id <<< "$flagged")" == "$authored_id" && "$(json_field messages.0.author_status <<< "$flagged")" == revoked ]] || { echo "unexpected poll: $flagged" >&2; exit 1; }
  echo "message $authored_id from $(json_field messages.0.sender_id <<< "$flagged"): author_status=revoked"
  cli ack --message "$authored_id"

  step "The pending work for $RAW_BOT is dispositioned instead of escalated"
  for _ in $(seq 1 30); do
    audit_has delivery.recipient_revoked attempted unknown_recipient && break
    sleep 0.5
  done
  audit_has delivery.recipient_revoked attempted unknown_recipient || { echo "pending work was not dispositioned" >&2; exit 1; }
  if audit_has message.unacknowledged_escalation attempted ""; then echo "revoked recipient's work escalated as unacknowledged" >&2; exit 1; fi
  for _ in $(seq 1 10); do grep -q '"delivery.recipient_revoked"' "$tmp_dir/notifications.log" && break; sleep 0.2; done
  grep -q '"delivery.recipient_revoked"' "$tmp_dir/notifications.log" || { echo "operator webhook missed delivery.recipient_revoked" >&2; exit 1; }
  echo "webhook: $(grep -o '{"code":"unknown_recipient".*"delivery.recipient_revoked"[^}]*}' "$tmp_dir/notifications.log" | head -1)"
  audit_has registry.changed recorded "" || { echo "registry change was not audited" >&2; exit 1; }
  audit_has auth.revoked_credential rejected agent_disabled || { echo "revoked credential use was not audited" >&2; exit 1; }

  step "Audit trail"
  audit_rows | python3 -c 'import json,sys
for e in reversed(json.load(sys.stdin)):
    d = e.get("detail") or {}
    extra = {k: d[k] for k in ("disabled", "key_id", "recipient_id", "space_id", "recipients", "memberships_changed") if d.get(k)}
    print(e["sequence"], e["action"], e.get("actor_id",""), e.get("subject_id","")[:8], e["outcome"], e.get("code",""), json.dumps(extra) if extra else "")'
fi

step "Walkthrough passed ($mode)"
