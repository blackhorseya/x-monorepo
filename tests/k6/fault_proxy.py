#!/usr/bin/env python3
"""Fault proxy for the k6 auth fixtures: forwards to the rpc, except that the first call whose
path ends with FAIL_PATH is answered 503 (a Connect `unavailable`) without reaching it.
  fault_proxy.py <listen port> <upstream port> <procedure, e.g. /identity.v1.IdentityService/WhoAmI>
REFLECT makes the injected answer echo the request's X-API-Key, as a misbehaving upstream or proxy
might — the scripts must not print it. 1: in the message and the X-Error-Code header after a word;
header: X-Error-Code is the key; json: the JSON code is the key; prefixed: "unavailable." + key in both;
redirect: a 307 whose Location carries the key (a client that follows it logs the URL).
Test tooling only: no product behaviour is changed, the rpc never sees the failed call."""
import http.server
import json
import os
import sys
import threading
import urllib.error
import urllib.request

listen, upstream, fail_path = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
lock = threading.Lock()
armed = True


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        global armed
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        with lock:
            inject = armed and self.path.endswith(fail_path)
            if inject:
                armed = False
        if inject:
            print(f"injected 503 on {self.path}", file=sys.stderr, flush=True)
            mode = os.environ.get("REFLECT", "")
            key = self.headers.get("X-API-Key", "") if mode else ""
            body = {"code": "unavailable", "message": "injected by fault_proxy.py"}
            extra = {}
            if mode == "1":
                body["message"] += " " + key
                extra["X-Error-Code"] = "unavailable " + key
            elif mode == "header":
                extra["X-Error-Code"] = key
            elif mode == "json":
                body["code"] = key
            elif mode == "prefixed":
                body["code"] = "unavailable." + key
                extra["X-Error-Code"] = "unavailable." + key
            if mode == "redirect":
                extra["Location"] = "http://127.0.0.1:9/?k=" + key
                self.reply(307, b"", extra)
                return
            self.reply(503, json.dumps(body).encode(), extra)
            return
        req = urllib.request.Request(f"http://127.0.0.1:{upstream}{self.path}", body, method="POST")
        for h in ("Content-Type", "X-API-Key"):
            if self.headers.get(h):
                req.add_header(h, self.headers[h])
        try:
            res = urllib.request.urlopen(req, timeout=30)
        except urllib.error.HTTPError as e:
            res = e
        extra = {k: v for k, v in res.headers.items() if k.lower() == "x-error-code"}
        self.reply(res.status if hasattr(res, "status") else res.code, res.read(), extra)

    def reply(self, status, body, extra):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for k, v in extra.items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", listen), Handler).serve_forever()
