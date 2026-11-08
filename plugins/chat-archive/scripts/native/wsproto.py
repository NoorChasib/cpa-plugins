"""Minimal RFC 6455 WebSocket framing for the native harness (stdlib only, no extensions).

The client side talks to CPA's Responses WebSocket route; the server side lets the mock
upstream act as a Codex WebSocket upstream. Text frames only, no compression; ping is
answered with pong, close ends the connection. Test-only, loopback-only.
"""
import base64
import hashlib
import json
import os
import socket
import struct

GUID = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def accept_key(key):
    return base64.b64encode(hashlib.sha1(key.encode() + GUID).digest()).decode()


class Closed(Exception):
    pass


class Conn:
    def __init__(self, sock, client, rfile=None):
        self.sock = sock
        self.client = client  # clients mask their frames
        self.rfile = rfile or sock.makefile("rb", buffering=0)

    def _read(self, n):
        out = b""
        while len(out) < n:
            chunk = self.rfile.read(n - len(out))
            if not chunk:
                raise Closed()
            out += chunk
        return out

    def send(self, payload, opcode=1):
        if isinstance(payload, str):
            payload = payload.encode()
        header = bytes([0x80 | opcode])
        mask_bit = 0x80 if self.client else 0
        n = len(payload)
        if n < 126:
            header += bytes([mask_bit | n])
        elif n < 1 << 16:
            header += bytes([mask_bit | 126]) + struct.pack(">H", n)
        else:
            header += bytes([mask_bit | 127]) + struct.pack(">Q", n)
        if self.client:
            mask = os.urandom(4)
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload)) if n < 1 << 16 else _mask_large(payload, mask)
            header += mask
        self.sock.sendall(header + payload)

    def send_json(self, value):
        self.send(json.dumps(value))

    def recv(self):
        """Return the next text or binary message; answers pings, raises Closed on close."""
        message = b""
        while True:
            b0, b1 = self._read(2)
            opcode, fin = b0 & 0x0F, b0 & 0x80
            n = b1 & 0x7F
            if n == 126:
                n = struct.unpack(">H", self._read(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self._read(8))[0]
            mask = self._read(4) if b1 & 0x80 else None
            data = self._read(n)
            if mask:
                data = _mask_large(data, mask)
            if opcode == 8:
                try:
                    self.send(data[:2], opcode=8)
                except OSError:
                    pass
                raise Closed()
            if opcode == 9:
                self.send(data, opcode=10)
                continue
            if opcode == 10:
                continue
            message += data
            if fin:
                return message

    def recv_json(self):
        return json.loads(self.recv())

    def close(self):
        try:
            self.send(struct.pack(">H", 1000), opcode=8)
        except OSError:
            pass
        try:
            self.sock.close()
        except OSError:
            pass


def _mask_large(data, mask):
    # XOR in 4-byte words; fast enough for the multi-MiB frames the memory case sends.
    n = len(data)
    pad = (-n) % 4
    word = int.from_bytes(mask * ((n + pad) // 4), "big")
    value = int.from_bytes(data + b"\0" * pad, "big") ^ word
    return value.to_bytes(n + pad, "big")[:n]


def connect(host, port, path, headers, timeout=30):
    """Open a client WebSocket to host:port path with extra request headers."""
    sock = socket.create_connection((host, port), timeout=timeout)
    key = base64.b64encode(os.urandom(16)).decode()
    lines = [f"GET {path} HTTP/1.1", f"Host: {host}:{port}", "Upgrade: websocket", "Connection: Upgrade",
             f"Sec-WebSocket-Key: {key}", "Sec-WebSocket-Version: 13"]
    lines += [f"{k}: {v}" for k, v in headers.items()]
    sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
    rfile = sock.makefile("rb", buffering=0)
    status = rfile.readline()
    if b" 101 " not in status:
        rest = b""
        try:
            rest = rfile.read(4096)
        except OSError:
            pass
        sock.close()
        raise RuntimeError("websocket upgrade refused: " + status.decode(errors="replace").strip() + " " + str(len(rest)))
    expected = accept_key(key)
    accepted = False
    while True:
        line = rfile.readline()
        if line in (b"\r\n", b"\n", b""):
            break
        name, _, value = line.decode().partition(":")
        if name.strip().lower() == "sec-websocket-accept" and value.strip() == expected:
            accepted = True
    if not accepted:
        sock.close()
        raise RuntimeError("websocket accept key mismatch")
    return Conn(sock, client=True, rfile=rfile)


def server_upgrade(handler):
    """Complete a server-side upgrade inside a BaseHTTPRequestHandler; returns a Conn."""
    key = handler.headers.get("Sec-WebSocket-Key", "")
    handler.send_response_only(101)
    handler.send_header("Upgrade", "websocket")
    handler.send_header("Connection", "Upgrade")
    handler.send_header("Sec-WebSocket-Accept", accept_key(key))
    handler.end_headers()
    handler.wfile.flush()
    handler.close_connection = True
    return Conn(handler.connection, client=False, rfile=handler.rfile)
