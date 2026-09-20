#!/usr/bin/env python3
"""Test signed feed discovery and rejection through the real Sparkle framework."""
import http.server
import pathlib
import subprocess
import sys
import tempfile
import threading

probe, bundle, feed = sys.argv[1:]
signed = pathlib.Path(feed).read_bytes()
# Same valid XML, different signed bytes. A parser accepting it isn't sufficient.
altered = signed.replace(b"</channel>", b" </channel>", 1)
assert signed != altered


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        content = signed if self.path == "/valid.xml" else altered
        self.send_response(200)
        self.send_header("Content-Type", "application/xml")
        self.send_header("Content-Length", str(len(content)))
        self.end_headers()
        self.wfile.write(content)

    def log_message(self, *args):
        pass


with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        for mode in ["valid", "invalid"]:
            subprocess.run([probe, str(pathlib.Path(bundle).resolve()),
                            f"http://127.0.0.1:{server.server_port}/{mode}.xml", mode],
                           check=True, timeout=45)
    finally:
        server.shutdown()
