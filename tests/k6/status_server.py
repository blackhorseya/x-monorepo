#!/usr/bin/env python3
"""A legal HTTP/JSON server for the k6 status-reflect fixture (wire.sh): every answer is a valid
200 + JSON, but one `status` field of the saga carries the request's X-API-Key, as a misbehaving
upstream or proxy that reflects the credential into an enum field would.
  status_server.py <port> <mode>
  reservation  GetReservation says status = <key>                  (reservation_wrong_status)
  order        GetReservation says RESERVED, GetOrder status = <key>   (order_not_confirmed)
  commit       GetReservation says RESERVED once per order, then <key>
               (GetOrder says CONFIRMED)                           (reservation_not_committed)
Prints `served <mode>` to stderr after each reflected answer. Test tooling only: the rpc is not involved."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, mode = int(sys.argv[1]), sys.argv[2]
reserved_seen = set()  # order ids whose first GetReservation was answered (mode commit)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        key = self.headers.get("X-API-Key", "")
        method = self.path.rsplit("/", 1)[-1]
        out, reflected = {}, False
        if method == "CreateOrder":
            out = {"order": {"id": "fake-order"}}
        elif method == "GetReservation":
            if mode == "reservation":
                out, reflected = {"reservation": {"status": key}}, True
            elif mode == "order":
                out = {"reservation": {"status": "RESERVATION_STATUS_RESERVED"}}
            else:
                first = body.get("order_id") not in reserved_seen
                reserved_seen.add(body.get("order_id"))
                status = "RESERVATION_STATUS_RESERVED" if first else key
                out, reflected = {"reservation": {"status": status}}, not first
        elif method == "GetOrder":
            if mode == "order":
                out, reflected = {"order": {"status": key}}, True
            else:
                out = {"order": {"status": "ORDER_STATUS_CONFIRMED"}}
        data = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
        if reflected:
            # wire.sh asserts this line: the reflected answer of this mode really went out
            print(f"served {mode}", file=sys.stderr, flush=True)


ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
