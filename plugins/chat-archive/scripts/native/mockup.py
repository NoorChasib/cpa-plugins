"""Loopback mock upstream for chat-archive native acceptance.

One ThreadingHTTPServer answers every provider CPA is configured with: Claude Messages, OpenAI
chat (openai-compatibility), Codex Responses over SSE and WebSocket, Gemini and Vertex
generateContent, and the xAI media routes. The case comes from the requested model (from the
body, or the Gemini path). Every response is synthetic and carries the run's canaries and
ordinary markers. The mock records request paths, header names and body sizes only.
"""
import base64
import json
import socket
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import wsproto


class State:
    def __init__(self, canaries):
        self.c = canaries
        self.lock = threading.Lock()
        self.seen = []          # (method, path without query, model, header names, body length)
        self.attempts = {}      # model -> count, for cases whose first attempt fails
        self.archive_headers = []  # x-archive-* header names that reached the upstream (must stay empty)
        self.unknown = []
        self.release = threading.Event()  # lets a held stream finish (reload case)

    def note(self, method, path, model, headers, length):
        names = sorted({k.lower() for k in headers.keys()})
        with self.lock:
            self.seen.append((method, path, model, names, length))
            self.archive_headers += [n for n in names if n.startswith("x-archive-")]

    def attempt(self, model):
        with self.lock:
            self.attempts[model] = self.attempts.get(model, 0) + 1
            return self.attempts[model]


def sse(data, event=None):
    text = data if isinstance(data, str) else json.dumps(data)
    return (("event: " + event + "\n") if event else "") + "data: " + text + "\n\n"


def make_handler(state):
    c = state.c
    M = c.markers
    S = c.secret
    B = c.body

    class Mock(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        # ---- plumbing ----
        def read_body(self):
            n = int(self.headers.get("Content-Length") or 0)
            raw = b""
            while len(raw) < n:
                chunk = self.rfile.read(min(n - len(raw), 1 << 20))
                if not chunk:
                    break
                raw += chunk
            return raw

        def common_headers(self):
            self.send_header("Set-Cookie", S["H5_set_cookie"])
            self.send_header("X-Api-Key", S["U7_echoed_api_key"])
            self.send_header("Anthropic-Ratelimit-Unified-Status", "allowed")
            self.send_header("Request-Id", "req_ca_mock")

        def reply(self, data, code=200, content_type="application/json", close=False):
            raw = data if isinstance(data, bytes) else data.encode()
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(raw)))
            if close:
                # CPA may not reuse an upstream connection, and each idle kept-alive connection holds
                # one handler thread here until it closes: load generators would exhaust the
                # container's thread limit.
                self.send_header("Connection", "close")
                self.close_connection = True
            self.common_headers()
            self.end_headers()
            self.wfile.write(raw)
            self.wfile.flush()

        def stream(self, frames, delay=0.004, broken=False, first_delay=0.0, hold=None, between=None):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.common_headers()
            self.end_headers()
            self.close_connection = True
            try:
                self.wfile.flush()
                if first_delay:
                    time.sleep(first_delay)
                for i, frame in enumerate(frames):
                    self.wfile.write(frame.encode() if isinstance(frame, str) else frame)
                    self.wfile.flush()
                    if hold is not None and i == hold:
                        state.release.wait(60)
                    if between is not None and i == between[0]:
                        time.sleep(between[1])
                    if delay:
                        time.sleep(delay)
                if broken:
                    self.connection.shutdown(socket.SHUT_RDWR)
            except (BrokenPipeError, ConnectionResetError, OSError):
                pass

        def drop(self, prefix_frames=()):
            """Send headers and some frames, then cut the connection (transport error)."""
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", "100000")
            self.end_headers()
            self.close_connection = True
            try:
                for frame in prefix_frames:
                    self.wfile.write(frame.encode())
                    self.wfile.flush()
                    time.sleep(0.01)
                self.connection.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

        # ---- routing ----
        def do_GET(self):
            path = urllib.parse.urlsplit(self.path).path
            state.note("GET", path, "", self.headers, 0)
            if self.headers.get("Upgrade", "").lower() == "websocket" and path.endswith("/responses"):
                self.websocket()
                return
            if "/videos/" in path:
                vid = path.rsplit("/", 1)[1]
                self.reply(json.dumps({"request_id": vid, "status": "done", "model": "grok-imagine-video",
                    "video": {"url": "https://vidgen.x.ai/ca-" + vid + ".mp4?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=" + S["M1_url_signature"],
                              "duration": 5, "respect_moderation": True}}))
                return
            with state.lock:
                state.unknown.append(("GET", path))
            self.reply('{"error":"not_found"}', code=404)

        def do_POST(self):
            parts = urllib.parse.urlsplit(self.path)
            path = parts.path
            raw = self.read_body()
            try:
                body = json.loads(raw) if raw[:1] in (b"{", b"[") else {}
            except ValueError:
                body = {}
            model = body.get("model", "") if isinstance(body, dict) else ""
            if ":" in path.rsplit("/", 1)[-1]:
                model = path.rsplit("/", 1)[-1].split(":", 1)[0]
            state.note("POST", path, model, self.headers, len(raw))
            if path.endswith("/v1/messages"):
                return self.claude(model, body)
            if path.endswith("/chat/completions") or "ca_q=" in parts.query:
                return self.openai(model, body)
            if path.endswith("/responses"):
                return self.codex(model, body)
            if ":generateContent" in path or ":streamGenerateContent" in path:
                return self.gemini(model, ":streamGenerateContent" in path)
            if path.endswith("/interactions"):
                return self.interactions(body)
            if ":countTokens" in path:
                return self.reply(json.dumps({"totalTokens": 12}))
            if path.endswith("/images/generations"):
                return self.reply(json.dumps({"created": int(time.time()), "data": [{"url": "https://imgen.x.ai/xai-imgen/ca-image.jpeg?X-Amz-Signature=" + S["M1_url_signature"], "revised_prompt": M["media"]}]}))
            if path.endswith("/videos/generations"):
                return self.reply(json.dumps({"request_id": "ca-video-1"}))
            if "speech" in path or path.endswith("/tts"):
                audio = b"ID3\x04\x00\x00\x00\x00\x00\x00" + bytes(range(256)) * 16
                return self.reply(audio, content_type="audio/mpeg")
            with state.lock:
                state.unknown.append(("POST", path))
            self.reply('{"error":"not_found"}', code=404)

        # ---- Claude ----
        def claude(self, model, body):
            key = self.headers.get("x-api-key", "") or self.headers.get("Authorization", "").removeprefix("Bearer ")
            if model == "ca-claude-failover" and key == c.claude_key_a:
                return self.reply(json.dumps({"type": "error", "error": {"type": "api_error",
                    "message": M["failure"] + " " + B["U8_failure_anthropic"] + " " + B["U8_failure_google"]}}), code=500)
            text = M["response"] + " R1 " + B["P9_github_pat"]
            if model == "ca-claude-bootstrap" and state.attempt(model) == 1:
                return self.drop([sse({"type": "ping"}, "ping")])
            if not body.get("stream"):
                return self.reply(json.dumps({"id": "msg_ca_" + model, "type": "message", "role": "assistant",
                    "model": "ca-physical-model", "content": [
                        {"type": "thinking", "thinking": M["thinking"], "signature": S["S1_thinking_signature"]},
                        {"type": "redacted_thinking", "data": S["S1_redacted_thinking"]},
                        {"type": "text", "text": text}],
                    "stop_reason": "end_turn", "usage": {"input_tokens": 11, "output_tokens": 7}}), close=model == "ca-claude-big")
            secret = B["R2_split_stream_secret"]
            half = len(secret) // 2
            frames = [sse({"type": "message_start", "message": {"id": "msg_ca_" + model, "type": "message", "role": "assistant",
                           "model": "ca-physical-model", "content": [], "usage": {"input_tokens": 11, "output_tokens": 1}}}, "message_start"),
                      sse({"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": ""}}, "content_block_start"),
                      sse({"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": M["thinking"]}}, "content_block_delta"),
                      sse({"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": S["S1_signature_delta"]}}, "content_block_delta"),
                      sse({"type": "content_block_stop", "index": 0}, "content_block_stop"),
                      sse({"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}}, "content_block_start"),
                      sse({"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": M["response"] + " R2 " + secret[:half]}}, "content_block_delta"),
                      sse({"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": secret[half:] + " end"}}, "content_block_delta")]
            if model == "ca-claude-midstream":
                frames.append(sse({"type": "error", "error": {"type": "overloaded_error", "message": "ca mid-stream " + B["R3_completion_error"]}}, "error"))
                return self.stream(frames)
            frames += [sse({"type": "content_block_stop", "index": 1}, "content_block_stop"),
                       sse({"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 9}}, "message_delta"),
                       sse({"type": "message_stop"}, "message_stop")]
            if model == "ca-claude-slow":
                return self.stream(frames, first_delay=3.0, between=(1, 4.0))
            if model == "ca-claude-hold":
                return self.stream(frames, hold=6)
            if model == "ca-claude-latency":
                many = frames[:6] + [sse({"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": "tok%03d " % i}}, "content_block_delta") for i in range(200)] + frames[-3:]
                return self.stream(many, delay=0.002)
            return self.stream(frames)

        # ---- OpenAI chat ----
        def openai(self, model, body):
            text = M["response"] + " R1 " + B["P15_gitlab"]
            enc = {"type": "reasoning.encrypted", "data": S["S2_reasoning_encrypted"], "index": 0, "format": "openai-responses-v1"}
            usage = {"prompt_tokens": 12, "completion_tokens": 6, "total_tokens": 18}
            if not body.get("stream"):
                return self.reply(json.dumps({"id": "chatcmpl-ca", "object": "chat.completion", "model": "ca-physical-model",
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": text, "reasoning_content": M["thinking"],
                                 "reasoning_details": [enc]}, "finish_reason": "stop"}], "usage": usage}))
            secret = B["R2_split_stream_secret"]
            half = len(secret) // 2

            def chunk(delta, finish=None):
                return sse({"id": "chatcmpl-ca", "object": "chat.completion.chunk", "model": "ca-physical-model",
                            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]})
            frames = [chunk({"role": "assistant", "reasoning_content": M["thinking"]}), chunk({"reasoning_details": [enc]}),
                      chunk({"content": M["response"] + " R2 " + secret[:half]}), chunk({"content": secret[half:]}),
                      chunk({}, "stop"), sse({"id": "chatcmpl-ca", "object": "chat.completion.chunk", "choices": [], "usage": usage}), sse("[DONE]")]
            return self.stream(frames)

        # ---- Codex Responses (HTTP SSE) ----
        def codex_events(self, model, rid, marker, empty_completed=False):
            secret = B["R2_split_stream_secret"]
            half = len(secret) // 2
            reasoning = {"type": "reasoning", "id": "rs_" + rid, "summary": [{"type": "summary_text", "text": M["thinking"]}],
                         "encrypted_content": S["S2_encrypted_content"]}
            message = {"type": "message", "id": "msg_" + rid, "role": "assistant", "status": "completed",
                       "content": [{"type": "output_text", "text": marker + " R2 " + secret + " end", "annotations": []}]}
            response = {"id": "resp_" + rid, "object": "response", "status": "in_progress", "model": "ca-physical-model", "output": []}
            events = [("response.created", {"type": "response.created", "response": response}),
                      ("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": {"type": "reasoning", "id": "rs_" + rid, "summary": []}}),
                      ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": reasoning}),
                      ("response.output_item.added", {"type": "response.output_item.added", "output_index": 1, "item": {"type": "message", "id": "msg_" + rid, "role": "assistant", "content": []}}),
                      ("response.output_text.delta", {"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "item_id": "msg_" + rid, "delta": marker + " R2 " + secret[:half]}),
                      ("response.output_text.delta", {"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "item_id": "msg_" + rid, "delta": secret[half:] + " end"}),
                      ("response.output_item.done", {"type": "response.output_item.done", "output_index": 1, "item": message})]
            done = dict(response, status="completed", output=[] if empty_completed else [reasoning, message],
                        usage={"input_tokens": 13, "output_tokens": 8, "total_tokens": 21})
            events.append(("response.completed", {"type": "response.completed", "response": done}))
            return events

        def codex(self, model, body):
            if model == "ca-codex-bootstrap" and state.attempt(model) == 1:
                # First attempt: an event whose JSON is cut short (ended by a blank line, so CPA's
                # Codex scanner delivers it alone), then a response.failed event. CPA forwards the
                # cut event as the first payload chunk (header-init, then chunk 0); its Responses
                # SSE validator holds it back because the JSON is incomplete, so no byte reaches the
                # client, and the failure arrives as a stream error before the first deliverable
                # chunk. CPA's bootstrap retry then runs the stream again, which sends a second
                # header-init for the same request.
                created = json.dumps({"type": "response.created", "response": {"id": "resp_boot_1", "object": "response",
                                      "status": "in_progress", "model": "ca-physical-model", "output": []}})
                failed = {"type": "response.failed", "response": {"id": "resp_boot_1", "status": "failed",
                          "error": {"code": "server_error", "message": "ca bootstrap failure"}}}
                return self.stream(["data: " + created[: len(created) // 2] + "\n\n", "data: " + json.dumps(failed) + "\n\n"])
            n = state.attempt("codex-http")
            rid = "%s_%d" % (model.replace("-", ""), n)
            marker = M["codex_native"] if model == "ca-codex-native" else M["response"]
            events = self.codex_events(model, rid, marker, empty_completed=model == "ca-codex-native")
            if body.get("stream") is False:
                final = events[-1][1]["response"]
                return self.reply(json.dumps(final))
            return self.stream([sse(e, name) for name, e in events])

        def websocket(self):
            conn = wsproto.server_upgrade(self)
            turn = 0
            try:
                while True:
                    request = conn.recv_json()
                    turn += 1
                    model = request.get("model", "")
                    state.note("WS", "/responses", model, {}, 0)
                    rid = "ws_%s_%d_%d" % (model.replace("-", ""), id(conn) % 10000, turn)
                    for _, event in self.codex_events(model, rid, M["websocket"]):
                        conn.send_json(event)
            except (wsproto.Closed, OSError, ValueError):
                pass
            finally:
                conn.close()

        # ---- Gemini / Vertex ----
        def gemini(self, model, streaming):
            thought = {"text": M["thinking"], "thought": True, "thoughtSignature": S["S2_thought_signature"]}
            secret = B["R2_split_stream_secret"]
            half = len(secret) // 2
            usage = {"promptTokenCount": 9, "candidatesTokenCount": 5, "totalTokenCount": 14}
            if not streaming:
                return self.reply(json.dumps({"candidates": [{"content": {"role": "model", "parts": [thought, {"text": M["response"] + " R1 " + B["P7_google"]}]},
                    "finishReason": "STOP", "index": 0}], "usageMetadata": usage, "modelVersion": "ca-physical-model", "responseId": "ca-gem-1"}))

            def chunk(parts, finish=None):
                cand = {"content": {"role": "model", "parts": parts}, "index": 0}
                if finish:
                    cand["finishReason"] = finish
                return sse({"candidates": [cand], "modelVersion": "ca-physical-model", "responseId": "ca-gem-2", **({"usageMetadata": usage} if finish else {})})
            frames = [chunk([thought]), chunk([{"text": M["response"] + " R2 " + secret[:half]}]), chunk([{"text": secret[half:]}], "STOP")]
            return self.stream(frames)

        # ---- Gemini Interactions (thought step and thought_signature stream deltas) ----
        def interactions(self, body):
            summary = [{"type": "text", "text": M["thinking"]}]
            usage = {"total_input_tokens": 9, "total_output_tokens": 5, "total_tokens": 14}
            if not body.get("stream"):
                return self.reply(json.dumps({"id": "int_ca_1", "object": "interaction", "status": "completed", "model": "ca-physical-model",
                    "steps": [{"type": "thought", "summary": summary, "signature": S["S2_interactions_signature"]},
                              {"type": "model_output", "content": [{"type": "text", "text": M["response"] + " R1 " + B["P16_huggingface"]}]}],
                    "usage": usage}))

            def ev(name, data):
                return sse(dict(data, event_type=name), name)
            frames = [ev("interaction.created", {"interaction": {"id": "int_ca_2", "status": "in_progress", "object": "interaction", "model": "ca-physical-model"}}),
                      ev("step.start", {"index": 0, "step": {"type": "thought"}}),
                      ev("step.delta", {"index": 0, "delta": {"type": "thought_summary", "content": summary[0]}}),
                      ev("step.delta", {"index": 0, "delta": {"type": "thought_signature", "signature": S["S2_interactions_signature_delta"]}}),
                      ev("step.stop", {"index": 0}),
                      ev("step.start", {"index": 1, "step": {"type": "model_output"}}),
                      ev("step.delta", {"index": 1, "delta": {"type": "text", "text": M["response"] + " R2 " + B["P17_npm"]}}),
                      ev("step.stop", {"index": 1}),
                      ev("interaction.completed", {"interaction": {"id": "int_ca_2", "status": "completed", "object": "interaction", "usage": usage}})]
            return self.stream(frames)

    return Mock


def start(canaries):
    state = State(canaries)
    server = ThreadingHTTPServer(("127.0.0.1", 0), make_handler(state))
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return state, server, thread


def b64(data):
    return base64.b64encode(data).decode()
