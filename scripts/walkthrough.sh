#!/usr/bin/env bash
# Replay the two-runtime walkthrough: a bot using the Go CLI and a bot using
# the standalone Python client exchange an instruction with a file reference,
# poll and acknowledge while offline, and reply with a correlated result.
#
#   scripts/walkthrough.sh          local: build, start inboxd on loopback,
#                                   replay, then revoke the Python bot and
#                                   show what the audit records about it
#   scripts/walkthrough.sh remote   replay against an existing server; set
#                                   INBOX_SERVER, CLI_BOT, CLI_KEY, RAW_BOT, RAW_KEY
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
  write_registry() {
    local raw_disabled="$1"
    cat > "$tmp_dir/registry.json.tmp" <<EOF
{"version":1,"agents":[
 {"id":"cli-bot","tenant_id":"walkthrough","public_keys":[{"id":"primary","public_key":"$(tr -d '\r\n' < "$tmp_dir/cli-bot.pub")"}],"allowed_recipients":["raw-bot"],"allowed_kinds":["instruction","result"]},
 {"id":"raw-bot","tenant_id":"walkthrough","disabled":$raw_disabled,"public_keys":[{"id":"primary","public_key":"$(tr -d '\r\n' < "$tmp_dir/raw-bot.pub")"}],"allowed_recipients":["cli-bot"],"allowed_kinds":["instruction","result"]}
]}
EOF
    mv "$tmp_dir/registry.json.tmp" "$tmp_dir/registry.json"
  }
  write_registry false
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

if [[ "$mode" == local ]]; then
  audit_rows() { "$tmp_dir/inboxd" audit --limit 100; }
  audit_has() { audit_rows | python3 -c 'import json,sys
action, outcome, code = sys.argv[1:4]
sys.exit(0 if any(e["action"] == action and e["outcome"] == outcome and e.get("code","") == code for e in json.load(sys.stdin)) else 1)' "$@"; }

  step "Work is left pending in both directions"
  pending="$(cli send --to "$RAW_BOT" --kind instruction --payload '{"task":"never collected"}')"
  pending_id="$(json_field id <<< "$pending")"
  authored="$(raw send --to "$CLI_BOT" --kind instruction --payload '{"task":"written before revocation"}')"
  authored_id="$(json_field id <<< "$authored")"
  echo "pending for $RAW_BOT: $pending_id; pending for $CLI_BOT: $authored_id"

  step "The operator disables $RAW_BOT (disable first, remove later)"
  write_registry true
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
  echo "webhook: $(grep -o '{"code".*"delivery.recipient_revoked"[^}]*}' "$tmp_dir/notifications.log" | head -1)"
  audit_has registry.changed recorded "" || { echo "registry change was not audited" >&2; exit 1; }
  audit_has auth.revoked_credential rejected agent_disabled || { echo "revoked credential use was not audited" >&2; exit 1; }

  step "Audit trail"
  audit_rows | python3 -c 'import json,sys
for e in reversed(json.load(sys.stdin)):
    d = e.get("detail") or {}
    extra = {k: d[k] for k in ("disabled", "key_id", "recipient_id") if d.get(k)}
    print(e["sequence"], e["action"], e.get("actor_id",""), e.get("subject_id","")[:8], e["outcome"], e.get("code",""), json.dumps(extra) if extra else "")'
fi

step "Walkthrough passed ($mode)"
