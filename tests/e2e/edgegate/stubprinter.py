#!/usr/bin/env python3
"""TCP stub ESC/POS printer for tests/e2e/edge-gate.sh.

usage: stubprinter.py <port-file> <capture-file>
Listens on a free loopback port (written to <port-file>), answers every
DLE EOT 1..4 status query with "paper ok" and appends every other byte to
<capture-file>.
"""
import socket
import sys
import threading

portfile, outfile = sys.argv[1], sys.argv[2]
Q = bytes([16, 4, 1, 16, 4, 2, 16, 4, 3, 16, 4, 4])
srv = socket.socket()
srv.bind(("127.0.0.1", 0))
srv.listen(8)
lock = threading.Lock()
out = open(outfile, "ab", buffering=0)
open(portfile, "w").write(str(srv.getsockname()[1]))


def put(b):
    with lock:
        out.write(b)


def serve(c):
    buf = b""
    while True:
        d = c.recv(4096)
        if not d:
            break
        buf += d
        while True:
            i = buf.find(Q)
            if i < 0:
                break
            put(buf[:i])
            buf = buf[i + len(Q):]
            c.sendall(bytes([0x12] * 4))
        if len(buf) > len(Q) - 1:
            put(buf[: -(len(Q) - 1)])
            buf = buf[-(len(Q) - 1):]
    put(buf)
    c.close()


while True:
    c, _ = srv.accept()
    threading.Thread(target=serve, args=(c,), daemon=True).start()
