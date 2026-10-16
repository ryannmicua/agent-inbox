#!/usr/bin/env bash
# Runs every `sh` block of docs/getting-started.md in order, in one shell, and
# checks each block's output against what the page tells the reader to expect.
#
# The commands run as the page prints them, with four changes that stand in
# for what the reader does by hand:
#
# - If GETTING_STARTED_REF is set to a commit, branch, or tag, step 1 installs
#   that revision instead of @latest, so CI tests the commit under review.
# - The foreground server runs in the background, with its log in a file.
# - A line that pastes an ID, such as MSG_ID=..., takes the first "id" from
#   the previous block's output, as the page tells the reader to copy it.
# - The script stops the server before the cleanup block, as the reader's
#   Ctrl-C does.
#
# HOME is a temporary directory, so the run touches neither ~/agent-inbox-demo
# nor ~/go. The Go module and build caches are the caller's.
#
# When a command or its output on the page changes, update the expectations
# below in the same commit.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
page="$repo_root/docs/getting-started.md"
module="github.com/ryannmicua/agent-inbox"
ref="${GETTING_STARTED_REF:-}"

command -v go >/dev/null || { echo "go is required" >&2; exit 1; }

# expect TEXT-IN-COMMAND TEXT-IN-OUTPUT...
# Each expectation applies to the first not yet checked block that contains
# TEXT-IN-COMMAND. An output text that starts with "!" must be absent.
expect_cmds=()
expect_outs=()
expect() {
  local cmd="$1"
  shift
  local out
  for out in "$@"; do
    expect_cmds+=("$cmd")
    expect_outs+=("$out")
  done
  expect_cmds+=("$cmd")
  expect_outs+=("")
}

expect 'go version' 'go version go1.'
expect 'go install' # exit status only
expect 'command -v agent-inbox' '/go/bin/agent-inbox'
expect 'agent-inbox version' 'agent-inbox v' 'inboxd v' 'agent-inbox-mcp v'
expect 'agent-inbox keygen' 'created Ed25519 keypair' 'private: agent-a.key' 'private: agent-b.key'
expect 'cat agent-a.pub' '='
expect 'cat > registry.json' '"id": "agent-a"' '"id": "agent-b"' '!"public_key": ""'
expect 'INBOX_WEBHOOK_URL=' 'agent-inbox listening on 127.0.0.1:8080'
expect 'inboxd healthcheck' 'healthy'
expect '--kind instruction' '"recipient_id":"agent-b"' '"payload":{"task":"say hello"}'
expect 'poll --agent agent-b' '"payload":{"task":"say hello"}'
expect 'ack --agent agent-b' '"acknowledged":true' '"duplicate":false'
expect 'poll --agent agent-b' '{"messages":[]}'
expect '--kind result' '"kind":"result"' '"reply_to":'
expect 'poll --agent agent-a' '"payload":{"reply":"hello back"}'
expect 'ack --agent agent-a' '"acknowledged":true'
expect 'agent-inbox-mcp --agent agent-a' '"serverInfo":{"name":"agent-inbox-mcp"' '"isError":false'
expect 'rm -rf ~/agent-inbox-demo' # exit status only
expect_used=()

work="$(mktemp -d)"
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# Keep the caller's Go caches and proxy settings, then give the run its own
# HOME, as on a fresh account.
GOMODCACHE="$(go env GOMODCACHE)"
GOCACHE="$(go env GOCACHE)"
GOPROXY="$(go env GOPROXY)"
GOSUMDB="$(go env GOSUMDB)"
export GOMODCACHE GOCACHE GOPROXY GOSUMDB
goprivate="$(go env GOPRIVATE)"
unset GOPATH GOBIN INBOX_SERVER
if [[ -n "$ref" ]]; then
  # Fetch the revision from the repository, not the module proxy, which may not
  # have seen a commit pushed moments ago.
  export GOPRIVATE="${goprivate:+$goprivate,}$module"
fi
export HOME="$work/home"
mkdir -p "$HOME"

mkdir -p "$work/blocks"
awk -v dir="$work/blocks" '
  /^## / { heading = substr($0, 4) }
  /^```sh$/ {
    n++
    file = sprintf("%s/%02d.sh", dir, n)
    printf "" > file
    print heading > sprintf("%s/%02d.title", dir, n)
    inside = 1
    next
  }
  inside && /^```$/ { inside = 0; close(file); next }
  inside { print > file }
' "$page"

failed=0
fail() {
  echo "FAIL: $*"
  failed=1
}

check_block() {
  local block="$1" out="$2" rc="$3" i matched="" text
  if [[ "$rc" != 0 ]]; then
    fail "exit status $rc"
  fi
  for i in "${!expect_cmds[@]}"; do
    [[ -z "${expect_used[$i]:-}" ]] || continue
    if [[ -z "$matched" ]]; then
      [[ "$block" == *"${expect_cmds[$i]}"* ]] || continue
      matched="${expect_cmds[$i]}"
    elif [[ "${expect_cmds[$i]}" != "$matched" ]]; then
      continue
    fi
    expect_used[i]=1
    text="${expect_outs[$i]}"
    [[ -n "$text" ]] || break
    if [[ "$text" == '!'* ]]; then
      if grep -qF -- "${text#!}" "$out"; then fail "output contains ${text#!}"; fi
    elif ! grep -qF -- "$text" "$out"; then
      fail "output lacks $text"
    fi
  done
}

prev_out=/dev/null
for file in "$work"/blocks/*.sh; do
  block="$(<"$file")"
  title="$(<"${file%.sh}.title")"
  out="${file%.sh}.out"
  run="$block"
  if [[ -n "$ref" ]]; then
    run="${run//@latest/@$ref}"
  fi

  # A pasted ID takes the first "id" from the previous block's output.
  pasted=""
  while IFS= read -r line; do
    if [[ "$line" =~ ^([A-Z_]+_ID)=[0-9a-f-]+$ ]]; then
      id="$(grep -o '"id":"[^"]*"' "$prev_out" | head -n 1 | cut -d'"' -f4)"
      line="${BASH_REMATCH[1]}=$id"
    fi
    pasted+="$line"$'\n'
  done <<<"$run"
  run="$pasted"

  echo "== $title"
  printf '%s\n' "$block" | sed 's/^/$ /'
  if [[ "${run%$'\n'}" != "$block" ]]; then
    echo "-- run as:"
    printf '%s' "$run" | sed 's/^/$ /'
  fi

  if [[ "$block" == *INBOX_WEBHOOK_URL=* && "$block" == *$'\n'inboxd ]]; then
    set +eu +o pipefail
    eval "${run%$'\n'} >\"\$work/server.log\" 2>&1 </dev/null &"
    server_pid=$!
    set -euo pipefail
    for _ in $(seq 1 30); do
      grep -q 'listening on' "$work/server.log" && break
      kill -0 "$server_pid" 2>/dev/null || break
      sleep 1
    done
    cp "$work/server.log" "$out"
    rc=0
    kill -0 "$server_pid" 2>/dev/null || rc="server exited"
  else
    if [[ "$block" == *'rm -rf ~/agent-inbox-demo'* && -n "$server_pid" ]]; then
      kill "$server_pid"
      wait "$server_pid" 2>/dev/null || true
      server_pid=""
    fi
    set +eu +o pipefail
    eval "$run" >"$out" 2>&1 </dev/null
    rc=$?
    set -euo pipefail
  fi

  cat "$out"
  check_block "$block" "$out" "$rc"
  if [[ -n "$ref" && "$ref" =~ ^[0-9a-f]{40}$ && "$block" == *'agent-inbox version'* ]]; then
    if [[ "$(grep -c -- "-${ref:0:12}\$" "$out")" != 3 ]]; then
      fail "the binaries were not built from ${ref:0:12}"
    fi
  fi
  if [[ "$failed" != 0 ]]; then
    if [[ -f "$work/server.log" ]]; then
      echo "== server log"
      cat "$work/server.log"
    fi
    exit 1
  fi
  prev_out="$out"
done

for i in "${!expect_cmds[@]}"; do
  if [[ -z "${expect_outs[$i]}" && -z "${expect_used[$i]:-}" ]]; then
    fail "no block on the page runs ${expect_cmds[$i]}"
  fi
done
if [[ "$failed" != 0 ]]; then
  exit 1
fi
if [[ -d "$HOME/agent-inbox-demo" ]]; then
  echo "FAIL: cleanup left $HOME/agent-inbox-demo"
  exit 1
fi
echo "== getting started: every block ran and matched the page"
