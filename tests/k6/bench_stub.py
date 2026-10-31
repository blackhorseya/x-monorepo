"""A stand-in for the rpc, for bench_fixture_test.sh: answers the calls bench.js makes (SetStock only for a SKU already upserted), with
delays the test chooses, and logs the SKU of every CreateOrder (one per line) to $STUB_SKU_LOG.
    python3 tests/k6/bench_stub.py <port>
Env: SETUP_DELAY_MS (each SetStock), CREATE_DELAY_MS (each CreateOrder), NEVER_RESERVE (GetReservation is 404
forever), STUB_SKU_LOG."""
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

setup_delay = int(os.environ.get("SETUP_DELAY_MS", "0")) / 1000
create_delay = int(os.environ.get("CREATE_DELAY_MS", "0")) / 1000
log_path = os.environ["STUB_SKU_LOG"]
lock = threading.Lock()
counter = [0]
products = set()  # SKUs UpsertProduct has seen: SetStock refuses the others, like the real rpc


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        method = self.path.rsplit("/", 1)[-1]
        out = {}
        if method == "UpsertProduct":
            with lock:
                products.add(body["sku"])
        elif method == "SetStock":
            with lock:
                known = body["sku"] in products
            if not known:
                return self.reply(400, {"code": "invalid_argument", "message": "the catalog has no product with this sku"})
            time.sleep(setup_delay)
        elif method == "CreateOrder":
            time.sleep(create_delay)
            with lock:
                counter[0] += 1
                out = {"order": {"id": f"o-{counter[0]}"}}
                with open(log_path, "a") as f:
                    f.write(body["items"][0]["sku"] + "\n")
        elif method == "GetReservation" and os.environ.get("NEVER_RESERVE"):
            return self.reply(404, {"code": "not_found", "message": "not yet"})
        elif method == "GetReservation":
            out = {"reservation": {"status": "RESERVATION_STATUS_RESERVED"}}
        self.reply(200, out)

    def reply(self, status, out):
        data = json.dumps(out).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
