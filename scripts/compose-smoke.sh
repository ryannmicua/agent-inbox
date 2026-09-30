#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

mode="${1:-sqlite}"
if [[ "$mode" != sqlite && "$mode" != postgres ]]; then
  echo "usage: $0 [sqlite|postgres]" >&2
  exit 2
fi

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

tmp_dir="$(mktemp -d)"
chmod 0755 "$tmp_dir"
project="agent-inbox-smoke-$$"
export INBOX_HOST_PORT="${INBOX_HOST_PORT:-18080}"
export INBOX_REGISTRY_FILE="$tmp_dir/registry.json"
export INBOX_WEBHOOK_URL="http://notification-sink:8081/notifications"

compose=(docker compose --project-name "$project" -f compose.yaml)
if [[ "$mode" == postgres ]]; then
  export POSTGRES_USER="agent_inbox"
  export POSTGRES_DB="agent_inbox"
  export POSTGRES_PASSWORD="agent-inbox-smoke/${RANDOM}?#%-$$"
  export POSTGRES_HOST_PORT="${POSTGRES_HOST_PORT:-15432}"
  url_password="$(POSTGRES_PASSWORD="$POSTGRES_PASSWORD" python3 -c 'import os; from urllib.parse import quote; print(quote(os.environ["POSTGRES_PASSWORD"], safe=""))')"
  export INBOX_DATABASE_URL="postgres://${POSTGRES_USER}:${url_password}@postgres:5432/${POSTGRES_DB}?sslmode=disable"
  compose+=(-f compose.postgres.yaml)
fi

cleanup() {
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

go build -trimpath -o "$tmp_dir/agent-inbox" ./cmd/agent-inbox
cli="$tmp_dir/agent-inbox"
"$cli" keygen --private-key "$tmp_dir/agent-a.key" --public-key "$tmp_dir/agent-a.pub"
"$cli" keygen --private-key "$tmp_dir/agent-b.key" --public-key "$tmp_dir/agent-b.pub"
agent_a_public="$(tr -d '\r\n' < "$tmp_dir/agent-a.pub")"
agent_b_public="$(tr -d '\r\n' < "$tmp_dir/agent-b.pub")"
cat > "$INBOX_REGISTRY_FILE" <<EOF
{
  "version": 1,
  "agents": [
    {"id":"agent-a","tenant_id":"smoke-tenant","public_keys":[{"id":"primary","public_key":"$agent_a_public"}],"allowed_recipients":["agent-b"],"allowed_kinds":["instruction","result"]},
    {"id":"agent-b","tenant_id":"smoke-tenant","public_keys":[{"id":"primary","public_key":"$agent_b_public"}],"allowed_recipients":["agent-a"],"allowed_kinds":["instruction","result"]}
  ]
}
EOF
chmod 0644 "$INBOX_REGISTRY_FILE"

"${compose[@]}" up --build -d
healthy=0
for attempt in $(seq 1 40); do
  if "${compose[@]}" exec -T server /usr/local/bin/inboxd healthcheck; then
    healthy=1
    break
  fi
  sleep 2
done
if [[ "$healthy" != 1 ]]; then
  "${compose[@]}" logs >&2
  echo "Compose server did not become healthy" >&2
  exit 1
fi

if [[ "$mode" == postgres ]]; then
  test_url="postgres://${POSTGRES_USER}:${url_password}@127.0.0.1:${POSTGRES_HOST_PORT}/${POSTGRES_DB}?sslmode=disable"
  INBOX_TEST_POSTGRES_URL="$test_url" go test ./...
fi

server="http://127.0.0.1:${INBOX_HOST_PORT}"
instruction="$("$cli" send --server "$server" --agent agent-a --key "$tmp_dir/agent-a.key" --to agent-b --kind instruction --payload '{"task":"prepare a result"}')"
read -r message_id task_id thread_id < <(python3 -c 'import json,sys; m=json.load(sys.stdin); print(m["id"],m["task_id"],m["thread_id"])' <<< "$instruction")

received="$("$cli" poll --server "$server" --agent agent-b --key "$tmp_dir/agent-b.key")"
python3 -c 'import json,sys; p=json.load(sys.stdin); assert len(p["messages"]) == 1; assert p["messages"][0]["id"] == sys.argv[1]; assert p["messages"][0]["tenant_id"] == "smoke-tenant"' "$message_id" <<< "$received"
"$cli" ack --server "$server" --agent agent-b --key "$tmp_dir/agent-b.key" --message "$message_id" >/dev/null

result="$("$cli" send --server "$server" --agent agent-b --key "$tmp_dir/agent-b.key" --to agent-a --kind result --task "$task_id" --thread "$thread_id" --reply-to "$message_id" --payload '{"result":"ready"}')"
result_id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <<< "$result")"
returned="$("$cli" poll --server "$server" --agent agent-a --key "$tmp_dir/agent-a.key")"
python3 -c 'import json,sys; p=json.load(sys.stdin); assert len(p["messages"]) == 1; m=p["messages"][0]; assert m["id"] == sys.argv[1]; assert m["reply_to"] == sys.argv[2]; assert m["kind"] == "result"' "$result_id" "$message_id" <<< "$returned"
"$cli" ack --server "$server" --agent agent-a --key "$tmp_dir/agent-a.key" --message "$result_id" >/dev/null

sink_logs="$("${compose[@]}" logs --no-color notification-sink)"
python3 -c 'import json,sys; events=[]
for line in sys.stdin:
    if " | " not in line: continue
    try: events.append(json.loads(line.split(" | ",1)[1]))
    except json.JSONDecodeError: pass
names={event.get("event") for event in events}
assert {"message.accepted","message.acknowledged"} <= names, events' <<< "$sink_logs"

echo "Compose $mode end-to-end exchange passed: instruction $message_id -> result $result_id"
