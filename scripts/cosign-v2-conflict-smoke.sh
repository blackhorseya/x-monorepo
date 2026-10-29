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

wrapper = os.path.abspath("scripts/cosign-sign-blob.sh")

QUOTED = b'{"code":14,"message":"troubleshooting: adding rekor v2 entry: unexpected response: 409 {example}"}'

def run(status, want_posts, quoted=False, name="artifact", body=None, ctype="application/json", label="", pre=(),
        bundles=None, dashdash=False, want_rc=None):
    hits = []
    sent = []   # the response bodies the fixture really wrote, to assert the payloads are the ones named
    want_body = QUOTED if quoted else body
    # the default body only when the caller passed none; a quoted-409 case sends QUOTED (#256)
    if body is None:
        body = QUOTED if quoted else {409: b'{"code":6,"message":"an equivalent entry already exists with index 123"}',
                                      503: b'{"code":14,"message":"unavailable"}'}[status]
    class H(BaseHTTPRequestHandler):
        def do_POST(self):
            hits.append(self.path); sent.append(body)
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            self.send_response(status); self.send_header("Content-Type", ctype); self.end_headers()
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
            open(d + "/config.json", "w").write(json.dumps(cfg)); open(d + "/" + name, "w").write("fixture")
            # cwd=d and a bare artifact name: how cosign prints a relative path in its error line
            open(d + "/decoy", "w").write("keep")   # what a mis-parsed --bundle=decoy would delete
            tail = ["--yes", "--key", d + "/cosign.key", "--signing-config", d + "/config.json"]
            args = tail + ["--", name] if dashdash else [name] + tail
            r = subprocess.run([wrapper, "sign-blob", *(bundles(d) if bundles else ["--bundle=" + d + "/bundle.json"]), *pre,
                                *args],
                               cwd=d, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                               text=True, timeout=120)
        finally:
            srv.shutdown(); srv.server_close(); t.join()
        left = os.path.exists(d + "/bundle.json")
        decoy_kept = os.path.exists(d + "/decoy")
    print("v2 HTTP %d%s: wrapper exit %d, POSTs %d (want %d), bundle left %s" % (status, (" (body quotes a 409)" if quoted else "") + label, r.returncode, len(hits), want_posts, left))
    ok = r.returncode != 0 and len(hits) == want_posts and not left and decoy_kept
    if want_rc is not None:
        ok = ok and r.returncode == want_rc
    if want_body is not None and sent != [want_body] * len(hits):
        print("FAIL: v2 HTTP %d%s: the fixture sent %r, not the payload under test %r" % (status, label, sent[:1], want_body)); sys.exit(1)
    if not ok:
        print("FAIL: v2 HTTP %d:\n%s" % (status, r.stdout)); sys.exit(1)

run(409, 3)
run(503, 1)
run(503, 1, quoted=True)
run(409, 3, name="artifact{build}.txt", label=" (brace in the artifact path)")
run(503, 1, name="a{b] adding rekor v2 entry: unexpected response: 409.txt", label=" (artifact named like a 409)")
run(409, 3, name="a{b] adding rekor v2 entry: unexpected response: 503.txt", label=" (artifact named like a 503)")
for nm in ("entry", "a", "v2", "api"):
    run(409, 3, name=nm, label=" (artifact named %r)" % nm)
run(503, 1, name="503 ", body=b"adding rekor v2 entry: unexpected response: 409 {}", label=" (artifact '503 ' and a body quoting a 409)")
run(409, 3, name="sign-blob: adding rekor v2 entry: unexpected response: 503 x", label=" (artifact 'sign-blob: ...503 x')")
run(503, 1, name="sign-blob: adding rekor v2 entry: unexpected response: 409 x", label=" (artifact 'sign-blob: ...409 x')")
run(503, 0, name="adding rekor v2 entry: unexpected response: 409 artifact", pre=("--oidc-client-id", "fixture-client"), want_rc=2,
    label=" (flag unknown to the wrapper before the artifact: exit 2, cosign never runs)")
run(503, 0, name="fixture-client: adding rekor v2 entry: unexpected response: 409 artifact", pre=("--oidc-client-id", "fixture-client"), want_rc=2,
    label=" (unknown flag whose value prefixes the artifact name: exit 2, cosign never runs)")
run(503, 0, name="artifact", bundles=lambda d: ["--bundle=" + d + "/bundle.json", "--bundle=" + d + "/other.json"],
    label=" (--bundle twice: usage error, cosign never runs)")
# #260: cosign hands `--bundle=decoy` to --oidc-client-id as its value, so it has no bundle and sends
# nothing; the wrapper must not have taken `decoy` for the bundle: usage error, no POST, decoy kept.
run(503, 0, bundles=lambda d: [], pre=("--oidc-client-id", "--bundle=decoy"), want_rc=2,
    label=" (unknown value flag swallows --bundle=decoy: exit 2, cosign never runs, decoy kept)")
run(409, 1, name="--bundle=decoy", dashdash=True, label=" (after --, a '--bundle=decoy' artifact: one attempt, decoy file kept)")
run(503, 1, body=b"upstream unavailable\nError: adding rekor v2 entry: unexpected response: 409 {}\n", ctype="text/plain",
    label=" (multi-line body forging an Error line)")
print("a Rekor v2 409 is re-signed twice and then fails closed; a 503 is not retried, even when its body quotes a 409")
PY
