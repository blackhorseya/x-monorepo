#!/usr/bin/env python3
"""A stand-in for the rpc server's identity.v1.IdentityService (Connect over JSON), just enough for
scripts/dex-prebind.sh: CreateAccount, ListAccounts, ListAccountIdentities, CreateInvite (admin API
key) and ClaimInvite (a bearer token). It keeps its state in memory, appends one line per call to
the log file ("<Method> <admin|bearer|none>", never a credential), and serves GET /_state (the
whole state as JSON) for the test to assert on. Tokens are "tok-secret-<email>"; scripts/test.sh
uses a fake dex-token.sh that prints the same.

  stub_rpc.py <port-file> <log-file>      # binds 127.0.0.1:0, writes the port to <port-file>

STUB_ADMIN_KEY (the admin key), STUB_ISSUER (the issuer a claim binds), STUB_FAIL_CLAIM=1 (every
ClaimInvite answers 500) come from the environment.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ADMIN_KEY = os.environ["STUB_ADMIN_KEY"]
ISSUER = os.environ["STUB_ISSUER"]
FAIL_CLAIM = os.environ.get("STUB_FAIL_CLAIM") == "1"
PREFIX = "/identity.v1.IdentityService/"
state = {"accounts": [], "invites": [], "identities": []}


def account(a):
    return {"id": a["id"], "name": a["name"], "roles": a["roles"], "status": "ACCOUNT_STATUS_ACTIVE"}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, body, code=None):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        if code:
            self.send_header("x-error-code", code)
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/_state":
            self.reply(200, state)
        else:
            self.reply(404, {"code": "not_found"})

    def do_POST(self):
        method = self.path[len(PREFIX):] if self.path.startswith(PREFIX) else None
        n = int(self.headers.get("Content-Length") or 0)
        req = json.loads(self.rfile.read(n) or b"{}")
        bearer = (self.headers.get("Authorization") or "").removeprefix("Bearer ")
        admin = self.headers.get("X-API-Key") == ADMIN_KEY
        kind = "admin" if admin else "bearer" if bearer else "none"
        with open(LOG, "a") as f:
            f.write(f"{method} {kind}\n")
        if method == "ClaimInvite":
            return self.claim(bearer)
        if method not in ("CreateAccount", "ListAccounts", "ListAccountIdentities", "CreateInvite"):
            return self.reply(404, {"code": "unimplemented"})
        if not admin:
            return self.reply(401, {"code": "unauthenticated", "message": "bad key"}, "auth.invalid_credential")
        getattr(self, method)(req)

    def CreateAccount(self, req):
        a = {"id": f"acc-{len(state['accounts']) + 1}", "name": req["name"], "roles": req.get("roles", [])}
        state["accounts"].append(a)
        self.reply(200, {"account": account(a)})

    def ListAccounts(self, req):
        size = req.get("pageSize") or 20
        start = int(req.get("pageToken") or 0)
        page = state["accounts"][start:start + size]
        body = {"accounts": [account(a) for a in page]}
        if start + size < len(state["accounts"]):
            body["nextPageToken"] = str(start + size)
        self.reply(200, body)

    def ListAccountIdentities(self, req):
        aid = req["accountId"]
        if not any(a["id"] == aid for a in state["accounts"]):
            return self.reply(404, {"code": "not_found"}, "account.not_found")
        self.reply(200, {
            "identities": [i for i in state["identities"] if i["accountId"] == aid],
            "invites": [i for i in state["invites"] if i["accountId"] == aid],
        })

    def CreateInvite(self, req):
        inv = {"id": f"inv-{len(state['invites']) + 1}", "email": req["email"], "accountId": req["accountId"],
               "status": "INVITE_STATUS_PENDING"}
        state["invites"].append(inv)
        self.reply(200, {"invite": inv})

    def claim(self, token):
        if not token.startswith("tok-secret-"):
            return self.reply(401, {"code": "unauthenticated"}, "auth.invalid_credential")
        if FAIL_CLAIM:
            return self.reply(500, {"code": "internal", "message": f"boom {token}"})
        email = token[len("tok-secret-"):]
        for inv in state["invites"]:
            if inv["email"] == email and inv["status"] == "INVITE_STATUS_PENDING":
                inv["status"] = "INVITE_STATUS_CLAIMED"
                state["identities"].append({"issuer": ISSUER, "subject": "sub-" + email, "accountId": inv["accountId"]})
                acc = next(a for a in state["accounts"] if a["id"] == inv["accountId"])
                return self.reply(200, {"account": account(acc)})
        self.reply(404, {"code": "not_found"}, "invite.not_found")


if __name__ == "__main__":
    LOG = sys.argv[2]
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    with open(sys.argv[1], "w") as f:
        f.write(str(srv.server_address[1]))
    srv.serve_forever()
