#!/usr/bin/env python3
"""A raw-socket server that answers every request with a broken HTTP/1.1 response, for the k6
wire fixture (wire.sh).
  wire_server.py <port> <mode>
Each mode puts the request's X-API-Key where Go's HTTP parser quotes it into its error, as a
misbehaving upstream or proxy that reflects the credential would:
  status     the status code is the key        (malformed HTTP status code "<key>")
  version    the HTTP version is the key       (malformed HTTP version "<key>")
  header     a header line without a colon     (malformed MIME header: missing colon: "Bad <key>")
  chunk      a chunked body whose size is the key
  truncated  Content-Length 100, 5 bytes, close (unexpected EOF; the key is only in a header)
  closed     the connection is closed without a byte: no key on the wire at all
Prints `served <mode>` to stderr after each answer. Test tooling only: the rpc is not involved."""
import socket
import sys
import threading

port, mode = int(sys.argv[1]), sys.argv[2]


def key_of(request):
    for line in request.decode("latin1").split("\r\n"):
        if line.lower().startswith("x-api-key:"):
            return line.split(":", 1)[1].strip()
    return ""


def answer(key):
    return {
        "status": f"HTTP/1.1 {key} OK\r\nContent-Length: 0\r\n\r\n",
        "version": f"{key} 200 OK\r\nContent-Length: 0\r\n\r\n",
        "header": f"HTTP/1.1 200 OK\r\nBad Header {key}\r\nContent-Length: 0\r\n\r\n",
        "chunk": f"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{key}\r\nx\r\n0\r\n\r\n",
        "truncated": f"HTTP/1.1 200 OK\r\nX-Echo: {key}\r\nContent-Length: 100\r\n\r\nshort",
        "closed": "",
    }[mode].encode("latin1")


def serve(conn):
    with conn:
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = conn.recv(65536)
            if not chunk:
                return
            data += chunk
        conn.sendall(answer(key_of(data)))
        # wire.sh asserts this line: the broken answer of this mode really went out
        print(f"served {mode}", file=sys.stderr, flush=True)


listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(("127.0.0.1", port))
listener.listen(128)
while True:
    c, _ = listener.accept()
    threading.Thread(target=serve, args=(c,), daemon=True).start()
