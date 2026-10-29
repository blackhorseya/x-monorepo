"""A stand-in for the rpc, for bench_fixture_test.sh: answers the four calls bench.js makes, with
delays the test chooses, and logs the SKU of every CreateOrder (one per line) to $STUB_SKU_LOG.
    python3 tests/k6/bench_stub.py <port>
Env: SETUP_DELAY_MS (each SetStock), CREATE_DELAY_MS (each CreateOrder), STUB_SKU_LOG."""
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


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        method = self.path.rsplit("/", 1)[-1]
        out = {}
        if method == "SetStock":
            time.sleep(setup_delay)
        elif method == "CreateOrder":
            time.sleep(create_delay)
            with lock:
                counter[0] += 1
                out = {"order": {"id": f"o-{counter[0]}"}}
                with open(log_path, "a") as f:
                    f.write(body["items"][0]["sku"] + "\n")
        elif method == "GetReservation":
            out = {"reservation": {"status": "RESERVATION_STATUS_RESERVED"}}
        data = json.dumps(out).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
