#!/usr/bin/env python3

import json
import logging
from http.server import BaseHTTPRequestHandler, HTTPServer

MAX_BODY_BYTES = 16 * 1024


class WebhookSink(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/healthz":
            self.send_error(404)
            return
        self.send_response(204)
        self.end_headers()

    def do_POST(self):
        if self.path != "/notifications":
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400)
            return
        if length < 1 or length > MAX_BODY_BYTES:
            self.send_error(413)
            return
        try:
            event = json.loads(self.rfile.read(length))
        except (json.JSONDecodeError, UnicodeDecodeError):
            self.send_error(400)
            return
        if not isinstance(event, dict):
            self.send_error(400)
            return
        logging.info(json.dumps(event, separators=(",", ":"), sort_keys=True))
        self.send_response(204)
        self.end_headers()

    def log_message(self, format_string, *args):
        return


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    HTTPServer(("0.0.0.0", 8081), WebhookSink).serve_forever()
