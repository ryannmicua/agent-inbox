#!/usr/bin/env python3
"""Drive an MCP stdio server through one session, as an agent runtime would.

    mcp_session.py COMMAND [ARG...] < calls.jsonl

Starts COMMAND, initializes an MCP session, then runs each tool call read
from stdin, one JSON object per line: {"name": "...", "arguments": {...}}.
Prints one JSON line per call: {"name", "is_error", "result"}, where result
is the call's structured content, or its text when it has none. Exits
non-zero if the server breaks the protocol. Standard library only.
"""

import json
import subprocess
import sys

PROTOCOL_VERSION = "2025-06-18"


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        return 2
    calls = [json.loads(line) for line in sys.stdin if line.strip()]
    server = subprocess.Popen(sys.argv[1:], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, encoding="utf-8")
    next_id = 0

    def request(method, params):
        nonlocal next_id
        next_id += 1
        server.stdin.write(json.dumps({"jsonrpc": "2.0", "id": next_id, "method": method, "params": params}) + "\n")
        server.stdin.flush()
        line = server.stdout.readline()
        if not line:
            raise SystemExit("mcp_session.py: server exited during %s" % method)
        reply = json.loads(line)
        if reply.get("jsonrpc") != "2.0" or reply.get("id") != next_id:
            raise SystemExit("mcp_session.py: unexpected reply to %s: %s" % (method, line.strip()))
        if "error" in reply:
            raise SystemExit("mcp_session.py: %s failed: %s" % (method, reply["error"]))
        return reply["result"]

    try:
        init = request("initialize", {"protocolVersion": PROTOCOL_VERSION, "capabilities": {},
                                      "clientInfo": {"name": "walkthrough", "version": "1"}})
        if init.get("protocolVersion") != PROTOCOL_VERSION or "tools" not in init.get("capabilities", {}):
            raise SystemExit("mcp_session.py: unexpected initialize result: %s" % init)
        server.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
        for call in calls:
            result = request("tools/call", {"name": call["name"], "arguments": call.get("arguments", {})})
            content = result.get("structuredContent")
            if content is None:
                content = "\n".join(c.get("text", "") for c in result.get("content", []))
            print(json.dumps({"name": call["name"], "is_error": bool(result.get("isError")), "result": content}), flush=True)
    finally:
        server.stdin.close()
        server.wait(timeout=10)
    return 0


if __name__ == "__main__":
    sys.exit(main())
