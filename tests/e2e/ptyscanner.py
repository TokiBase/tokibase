#!/usr/bin/env python3
"""A pty pair acting as a serial barcode scanner for tests/e2e/scanner.sh.

usage: ptyscanner.py <slave-path-file> <fifo>
Creates the pty, writes the slave device path to <slave-path-file>, then sends
every line read from <fifo> to the master as "<line>\\r" (what a scanner with a
CR terminator does). The line "@exit" ends the helper. The slave stays open so
the master never sees a hangup before the server attaches.
"""
import os
import sys

master, slave = os.openpty()
with open(sys.argv[1], "w") as f:
    f.write(os.ttyname(slave))
fifo = os.open(sys.argv[2], os.O_RDWR)  # O_RDWR: never EOF while we hold it
buf = b""
while True:
    chunk = os.read(fifo, 4096)
    if not chunk:
        break
    buf += chunk
    while b"\n" in buf:
        line, buf = buf.split(b"\n", 1)
        if line == b"@exit":
            sys.exit(0)
        os.write(master, line + b"\r")
