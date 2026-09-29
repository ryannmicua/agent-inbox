#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

tmp_dir="$(mktemp -d)"
project="agent-inbox-smoke-$$"
export INBOX_HOST_PORT="${INBOX_HOST_PORT:-18080}"
export INBOX_REGISTRY_FILE="$tmp_dir/registry.json"
export INBOX_WEBHOOK_URL="http://127.0.0.1:9"
compose=(docker compose --project-name "$project" -f compose.yaml)

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
  "${compose[@]}" logs server >&2
  echo "Compose server did not become healthy" >&2
  exit 1
fi

server="http://127.0.0.1:${INBOX_HOST_PORT}"
instruction="$("$cli" send --server "$server" --agent agent-a --key "$tmp_dir/agent-a.key" --to agent-b --kind instruction --payload '{"task":"prepare a result"}')"
read -r message_id task_id thread_id < <(python3 -c 'import json,sys; m=json.load(sys.stdin); print(m["id"],m["task_id"],m["thread_id"])' <<< "$instruction")

received="$("$cli" poll --server "$server" --agent agent-b --key "$tmp_dir/agent-b.key")"
python3 -c 'import json,sys; p=json.load(sys.stdin); assert len(p["messages"]) == 1; assert p["messages"][0]["id"] == sys.argv[1]; assert p["messages"][0]["tenant_id"] == "smoke-tenant"' "$message_id" <<< "$received"
"$cli" ack --server "$server" --agent agent-b --key "$tmp_dir/agent-b.key" --message "$message_id" --note "processed by Compose smoke test" >/dev/null

result="$("$cli" send --server "$server" --agent agent-b --key "$tmp_dir/agent-b.key" --to agent-a --kind result --task "$task_id" --thread "$thread_id" --reply-to "$message_id" --payload '{"result":"ready"}')"
result_id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <<< "$result")"
returned="$("$cli" poll --server "$server" --agent agent-a --key "$tmp_dir/agent-a.key")"
python3 -c 'import json,sys; p=json.load(sys.stdin); assert len(p["messages"]) == 1; m=p["messages"][0]; assert m["id"] == sys.argv[1]; assert m["reply_to"] == sys.argv[2]; assert m["kind"] == "result"' "$result_id" "$message_id" <<< "$returned"
"$cli" ack --server "$server" --agent agent-a --key "$tmp_dir/agent-a.key" --message "$result_id" --note "result processed" >/dev/null

echo "Compose end-to-end exchange passed: instruction $message_id -> result $result_id"
