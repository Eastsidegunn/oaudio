#!/usr/bin/env python3
"""A complete Oaudio runner in Python, standard library only.

It "recognizes" speech by reporting how loud the audio is, which is enough
to show every part of the protocol a recognizer needs: the arguments, the
handshake, /v1/info, a streamed NDJSON answer, errors, one request at a
time, and exiting when stdin closes. Replace `recognize` with a real model.

  go run ./tools/runner-check --manifest docs/examples/runner-manifest.json --model-dir . -- python3 docs/examples/runner.py
"""
import argparse, json, struct, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

args = argparse.ArgumentParser()
args.add_argument("--manifest", required=True)
args.add_argument("--model-dir", required=True)
args = args.parse_args()
manifest = json.load(open(args.manifest))
# Load the model here, from args.model_dir and manifest["engine_config"].
# Loading may take up to five minutes; print nothing on stdout meanwhile.

lock = threading.Lock()  # the daemon sends one request at a time; be safe anyway


def recognize(samples, rate):
    """Yields (start, end, text) per second of audio."""
    for i in range(0, len(samples), rate):
        chunk = samples[i:i + rate]
        loud = max((abs(s) for s in chunk), default=0)
        yield i / rate, (i + len(chunk)) / rate, f"[{loud}] "


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *a):  # logs go to stderr, as here, never stdout
        sys.stderr.write("runner: " + fmt % a + "\n")

    def answer(self, status, obj, ctype="application/json"):
        body = (json.dumps(obj) + "\n").encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/v1/info":
            return self.answer(200, {"protocol": 1, "engine": "example", "engine_version": "1", "tasks": ["stt"]})
        self.answer(404, {"error": "not found"})

    def do_POST(self):
        url = urlparse(self.path)
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if url.path != "/v1/transcribe":
            return self.answer(501, {"error": "this runner only transcribes"})
        q = {k: v[0] for k, v in parse_qs(url.query).items()}
        rate = int(q.get("sample_rate", 0))
        if rate <= 0:
            return self.answer(400, {"error": "sample_rate is required"})
        if q.get("task") == "translate":
            return self.answer(400, {"error": "this model does not translate"})
        samples = struct.unpack(f"<{len(body) // 2}h", body[:len(body) // 2 * 2])
        with lock:
            # Status and headers now; then one JSON line per segment, as it is
            # ready; then exactly one "done" (or "error" if it fails midway).
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()

            def line(obj):
                data = (json.dumps(obj, ensure_ascii=False) + "\n").encode()
                self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
                self.wfile.flush()

            for start, end, text in recognize(samples, rate):
                line({"type": "segment", "segment": {"start": start, "end": end, "text": text}})
            line({"type": "done", "language": q.get("language", "")})
            self.wfile.write(b"0\r\n\r\n")


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
# The handshake: one line, only once the model is loaded and the port open.
print(json.dumps({"oaudio_runner": 1, "addr": "127.0.0.1:%d" % server.server_address[1]}), flush=True)
sys.stdin.read()  # returns when the daemon closes our stdin: time to go
server.shutdown()
