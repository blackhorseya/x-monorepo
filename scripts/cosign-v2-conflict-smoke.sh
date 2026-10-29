#!/usr/bin/env bash
# The real cosign against a loopback Rekor v2 (#215): a 409 from POST /api/v2/log/entries must make
# scripts/cosign-sign-blob.sh sign again, three attempts in all and then a non-zero exit with no
# bundle left; a 503 must not be retried (one POST). The fake-cosign cases in scripts/test.sh pin
# the matcher against strings; this pins the string cosign v3.1.4 (sigstore-go v1.3.0, rekor-tiles
# v2.3.0 HTTP writer: "adding rekor v2 entry: unexpected response: 409 <body>") really prints.
# Offline apart from cosign itself. Needs cosign (the one release.yml pins) and python3. Locally:
# scripts/cosign-v2-conflict-smoke.sh
set -euo pipefail
cd "$(dirname "$0")/.."
exec python3 -I - <<'PY'
import json, os, subprocess, sys, tempfile, threading
from http.server import BaseHTTPRequestHandler, HTTPServer

def run(status, want_posts):
    hits = []
    body = {409: b'{"code":6,"message":"an equivalent entry already exists with index 123"}',
            503: b'{"code":14,"message":"unavailable"}'}[status]
    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            hits.append(self.path)
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            self.send_response(status); self.send_header("Content-Type", "application/json"); self.end_headers()
            self.wfile.write(body)
        def log_message(self, *a): pass
    with tempfile.TemporaryDirectory(prefix="cosign-v2-") as d:
        env = dict(os.environ, COSIGN_PASSWORD="", COSIGN_SIGN_RETRY_DELAY="0")
        r = subprocess.run(["cosign", "generate-key-pair"], cwd=d, env=env, stdin=subprocess.DEVNULL,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=60)
        assert r.returncode == 0, r.stdout
        srv = HTTPServer(("127.0.0.1", 0), H)
        t = threading.Thread(target=srv.serve_forever); t.start()
        try:
            cfg = {"mediaType": "application/vnd.dev.sigstore.signingconfig.v0.2+json",
                   "rekorTlogUrls": [{"url": "http://127.0.0.1:%d" % srv.server_port, "majorApiVersion": 2,
                                      "validFor": {"start": "2026-01-01T00:00:00Z"}, "operator": "fixture"}],
                   "rekorTlogConfig": {"selector": "ALL"}}
            open(d + "/config.json", "w").write(json.dumps(cfg)); open(d + "/artifact", "w").write("fixture")
            r = subprocess.run(["scripts/cosign-sign-blob.sh", "sign-blob", "--bundle=" + d + "/bundle.json",
                                d + "/artifact", "--yes", "--key", d + "/cosign.key",
                                "--signing-config", d + "/config.json"],
                               env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                               text=True, timeout=120)
        finally:
            srv.shutdown(); srv.server_close(); t.join()
        left = os.path.exists(d + "/bundle.json")
    print("v2 HTTP %d: wrapper exit %d, POSTs %d (want %d), bundle left %s" % (status, r.returncode, len(hits), want_posts, left))
    ok = r.returncode != 0 and len(hits) == want_posts and not left
    if not ok:
        print("FAIL: v2 HTTP %d:\n%s" % (status, r.stdout)); sys.exit(1)

run(409, 3)
run(503, 1)
print("a Rekor v2 409 is re-signed twice and then fails closed; a 503 is not retried")
PY
