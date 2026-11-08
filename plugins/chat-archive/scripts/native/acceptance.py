#!/usr/bin/env python3
"""chat-archive native acceptance on real CPA: every capture case, the canary scans and the memory,
latency and unreachable-archive checks.

CPA built from the pinned v8.0.60 source runs in a --network none container with a loopback
mock upstream (mockup.py) and the clienttest fake archive (fakearchive). Every value is
synthetic. Output holds case names, counts, sizes, timings and canary ids only.

  acceptance.py --cpa CPA --library PROD.so --reload-library PROD-0.1.1.so --fixture-library FIX.so
                --reject-library reject-fixture.so --fakearchive BIN [--only PHASE,...]
"""
import argparse
import base64
import hashlib
import json
from pathlib import Path
import shutil
import sys
import tempfile
import threading
import time
import traceback

import canaries as canarylib
import harness as H
import http_contract
import mockup
import wsproto

RESULTS = []

# The plugin's default memory-limit-mib. The memory phase runs with it and asserts the peak
# plugin runtime.sys stays below it.
DEFAULT_MEMORY_LIMIT_MIB = 768


def record(name, ok, detail=""):
    RESULTS.append({"case": name, "result": "PASS" if ok else "FAIL", "detail": detail})
    print(("PASS " if ok else "FAIL ") + name + ((": " + detail) if detail else ""), flush=True)


def run_case(name, fn, *args):
    try:
        detail = fn(*args)
        record(name, True, detail or "")
        return True
    except Exception as error:  # every failure is reported, the run continues
        msg = str(error) if isinstance(error, H.Failure) else type(error).__name__ + ": " + str(error)[:300]
        record(name, False, msg)
        if not isinstance(error, H.Failure):
            traceback.print_exc(limit=4)
        return False


class Run:
    """One CPA configuration with its directory, fake archive and expectations."""

    def __init__(self, args, c, mock_state, mock_url, name, library, plugin_overrides=None, reject=False, steering=False,
                 archive_url=None, plugins=True):
        self.args, self.c, self.mock, self.name = args, c, mock_state, name
        self.dir = Path(tempfile.mkdtemp(prefix="ca-" + name + "-", dir="/tmp"))
        self.dir.chmod(0o700)
        (self.dir / "plugins").mkdir()
        (self.dir / "auth").mkdir()
        self.fake = H.Fake(args.fakearchive, self.dir)
        c.source_token = self.fake.token
        exact = [canarylib.exact_fp(c.ops_password)]
        self.fake.post("/exact", {"rev": 7, "fps": exact, "caller_fps": [canarylib.caller_scope(c.client_key), canarylib.caller_scope(c.client_key_2)]})
        if plugins:
            shutil.copy2(library, self.dir / "plugins" / "chat-archive.so")
        extra = None
        if reject:
            shutil.copy2(args.reject_library, self.dir / "plugins" / "reject-fixture.so")
            extra = {"reject-fixture": {"enabled": True, "priority": 200}}
        self.config = H.cpa_config(self.dir, H.free_port(), mock_url, c, archive_url or self.fake.url, extra, plugin_overrides,
                                   steering, plugins)
        self.cpa = H.CPA(args.cpa, self.dir, self.config, c)
        self.mgmt_outputs = []  # every management/public response body, scanned at the end

    def start(self, wait_plugin=True):
        self.cpa.start(wait_plugin)
        return self

    @property
    def base(self):
        return self.cpa.base

    def status(self):
        status, raw = self.cpa.status()
        self.mgmt_outputs.append(raw)
        return status

    def data_dir(self):
        return self.dir / "plugins" / "data" / "chat-archive"

    def close(self):
        try:
            if self.cpa.proc is not None:
                self.cpa.stop(check_exit=False)
        finally:
            self.fake.close()


# ---------------------------------------------------------------------------------------------
# Client requests


def claude_body(c, model, stream, text=None, extra_content=None):
    content = [{"type": "text", "text": text or c.content_secrets_text()}]
    content += extra_content or []
    return {"model": model, "max_tokens": 64, "stream": stream,
            "system": [{"type": "text", "text": "ca system " + c.markers["request"]}],
            "messages": [{"role": "user", "content": content}]}


def send_claude(run, model, stream, **kw):
    code, _, raw = H.request(run.base, "/v1/messages", body=claude_body(run.c, model, stream, **kw),
                             headers=H.client_headers(run.c, "x-api-key"), timeout=120)
    return code, raw


def send_openai(run, model, stream):
    body = {"model": model, "stream": stream, "messages": [{"role": "system", "content": "ca system"},
            {"role": "user", "content": run.c.content_secrets_text()}]}
    code, _, raw = H.request(run.base, "/v1/chat/completions", body=body, headers=H.client_headers(run.c), timeout=120)
    return code, raw


def send_responses(run, model, stream):
    body = {"model": model, "stream": stream, "instructions": "ca instructions",
            "input": [{"role": "user", "content": [{"type": "input_text", "text": run.c.content_secrets_text()}]}]}
    code, _, raw = H.request(run.base, "/v1/responses", body=body, headers=H.client_headers(run.c), timeout=120)
    return code, raw


def send_gemini(run, model, stream):
    body = {"contents": [{"role": "user", "parts": [{"text": run.c.content_secrets_text()}]}],
            "systemInstruction": {"parts": [{"text": "ca system"}]}}
    path = "/v1beta/models/%s:%s" % (model, "streamGenerateContent?alt=sse" if stream else "generateContent")
    code, _, raw = H.request(run.base, path, body=body, headers=H.client_headers(run.c, "x-goog-api-key"), timeout=120)
    return code, raw


def send_interactions(run, model, stream):
    body = {"model": model, "stream": stream, "system_instruction": "ca system",
            "input": [{"type": "user_input", "content": [{"type": "text", "text": run.c.content_secrets_text()}]}],
            "generation_config": {"thinking_summaries": "auto"}}
    code, _, raw = H.request(run.base, "/v1beta/interactions", body=body, headers=H.client_headers(run.c, "x-goog-api-key"), timeout=120)
    return code, raw


# ---------------------------------------------------------------------------------------------
# Envelope helpers


class View:
    def __init__(self, fake):
        self.rows = fake.envelopes()
        self.by_kind = {}
        for status, reason, env in self.rows:
            self.by_kind.setdefault(env["kind"], []).append((status, reason, env))

    def kind(self, kind):
        return [env for _, _, env in self.by_kind.get(kind, [])]

    def begins(self, model, stream=None):
        return [e for e in self.kind("exchange.begin") if e["body"].get("requested_model") == model
                and (stream is None or e["body"].get("stream") == stream)]

    def of(self, kind, rid):
        return [e for e in self.kind(kind) if e["body"].get("request_id") == rid]

    def completions_for(self, model, stream=None):
        return [e for e in self.kind("exchange.completion") if e["body"].get("requested_model") == model
                and (stream is None or e["body"].get("stream") == stream)]


def exchange(view, model, stream, outcome="succeeded", need_response=True):
    begins = view.begins(model, stream)
    H.check(begins, "no begin for", model, "stream" if stream else "nonstream")
    rid = begins[-1]["body"]["request_id"]
    comps = view.of("exchange.completion", rid)
    H.check(comps, "no completion for", model)
    comp = comps[-1]["body"]
    H.check(comp["outcome"] == outcome, model, "outcome", comp["outcome"], "want", outcome)
    H.check(comp["begin_written"] is True, model, "begin_written false")
    responses = view.of("exchange.response", rid)
    if need_response:
        H.check(responses, "no response for", model)
    attempts = view.of("exchange.attempt", rid)
    H.check(attempts, "no attempt for", model)
    begin = begins[-1]["body"]
    H.check(not begin["body"].get("dropped"), model, "begin dropped", begin["body"].get("dropped"))
    H.check(begin["body"].get("n", 0) > 0 and (begin["body"].get("items") or begin["body"].get("base")), model, "begin has no items")
    return begin, [r["body"] for r in responses], comp, [a["body"] for a in attempts]


def piece_text(fake, hashes):
    out = b""
    for h in hashes:
        try:
            p = fake.get("/piece?h=" + h)
            out += base64.b64decode(p["raw"]) + b"\n"
        except Exception:
            pass
    return out


def refs_of(env):
    return env.get("refs") or []


def settle(run, expect_quiet=2.0, timeout=90):
    """Wait until the outbox has nothing pending and nothing new arrived for expect_quiet s."""
    last = [-1, time.monotonic()]

    def quiet():
        status = run.status()
        pending = status.get("outbox", {}).get("pending", 1)
        n = len(run.fake.get("/envelopes") or [])
        if n != last[0]:
            last[0], last[1] = n, time.monotonic()
            return False
        return pending == 0 and time.monotonic() - last[1] >= expect_quiet
    H.wait_until(quiet, timeout, "outbox settled", 0.5)


# ---------------------------------------------------------------------------------------------
# Phase: main capture run


def phase_main(args, c, mock_state, mock_url, rules):
    run = Run(args, c, mock_state, mock_url, "main", args.library, reject=True)
    try:
        run.start()
        H.wait_until(lambda: run.status().get("uploader", {}).get("archive_id"), 30, "first sources/self sync")
        time.sleep(0.5)  # masker rebuild with the server exact set happens in the same sync
        run_case("auth: private routes 401/403 without or with a wrong key; public shell fixed", auth_checks, run)
        run_case("model-list GETs ignored by both hooks (counter only)", model_lists, run)
        sent = {}

        def send(name, fn, *a):
            code, raw = fn(run, *a)
            sent[name] = code
            H.check(code == 200, name, "HTTP", code)
        for model, stream in (("ca-claude", False), ("ca-claude", True)):
            run_case(f"send claude {'stream' if stream else 'non-stream'}", send, f"claude-{stream}", send_claude, model, stream)
        for stream in (False, True):
            run_case(f"send openai {'stream' if stream else 'non-stream'}", send, f"openai-{stream}", send_openai, "ca-openai", stream)
            run_case(f"send openai-response {'stream' if stream else 'non-stream'}", send, f"responses-{stream}", send_responses, "ca-codex", stream)
            run_case(f"send gemini {'stream' if stream else 'non-stream'}", send, f"gemini-{stream}", send_gemini, "ca-gemini", stream)
        run_case("send vertex non-stream (U3)", send, "vertex", send_gemini, "ca-vertex", False)
        for stream in (False, True):
            run_case(f"send interactions {'stream' if stream else 'non-stream'}", send, f"interactions-{stream}", send_interactions, "ca-interactions", stream)
        run_case("send openai-compat with userinfo/query BaseURL (U10)", send, "userinfo", send_openai, "ca-openai-userinfo", False)
        run_case("send native Codex fixture with empty response.completed.output", send, "codex-native", send_responses, "ca-codex-native", True)
        run_case("send failed upstream 500 then second auth", send, "failover", send_claude, "ca-claude-failover", False)
        pdf = [{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": base64.b64encode(fake_pdf()).decode()}}]
        run_case("send PDF document (claude)", send, "pdf", lambda r: send_claude(r, "ca-claude", False, text="pdf " + c.markers["request"], extra_content=pdf))
        run_case("send mid-stream error", midstream, run)
        run_case("send canceled before the first chunk", cancel_before, run)
        run_case("send canceled after the first chunk", cancel_after, run)
        run_case("send bootstrap retry", send, "bootstrap", send_responses, "ca-codex-bootstrap", True)
        run_case("send rejected by higher-priority test plugin", rejected, run)
        run_case("send Responses WebSocket merged (HTTP upstream)", websocket, run, "ca-codex-merged", 2)
        run_case("send Responses WebSocket native (upstream WebSocket)", websocket, run, "ca-codex-ws", 2)
        run_case("send image generation", media_image, run)
        run_case("send video create and poll", media_video, run)
        run_case("send speech", media_speech, run)
        settle(run)
        view = View(run.fake)
        verify_cases(run, view, c)
        run_case("canaries: secrets absent from uploads, pieces and heartbeats in every encoding", scan_uploads, run, c, rules)
        run_case("canaries: X-Archive-* never reached the mock upstream (any format)", lambda: H.check(not mock_state.archive_headers, "x-archive headers upstream", sorted(set(mock_state.archive_headers))) or "requests=%d" % len(mock_state.seen))
        run_case("outbox while CPA runs: live copy with a non-empty -wal, secrets absent raw and decoded, markers present", scan_outbox_live, run, c)
        run_case("reload: new version file while a stream is open", reload_case, run)
        before = len(run.fake.get("/envelopes"))
        code = run.cpa.stop(check_exit=False)
        record("clean exit 0 (SIGTERM)", code == 0, "exit=%s" % code)
        run_case("outbox: secrets absent, markers present, modes 0700/0600, integrity_check", scan_outbox, run, c)
        run_case("canaries: CPA log, management and status responses hold no secret and no marker", scan_logs_mgmt, run, c)
        run_case("restart with no duplicate envelope (have/need confirms)", restart_case, run, before)
        dump_meta(run)
    finally:
        run.close()


def dump_meta(run):
    """Metadata of every envelope (ids, kinds, outcomes, fingerprints; never content) for debugging."""
    rows = []
    for status, reason, env in run.fake.envelopes():
        b = env["body"]
        keep = {k: b.get(k) for k in ("request_id", "requested_model", "model", "stream", "kind", "transport", "input_seq", "outcome",
                                      "status_code", "begin_written", "ordinal", "auth_fp", "to_format", "provider", "failed",
                                      "failure_status", "account_fp", "account_label", "source_kind", "terminal_status",
                                      "bootstrap_resets", "index", "chunks", "truncated", "exec_session_id") if k in b}
        if "body" in b and isinstance(b["body"], dict):
            keep["n"] = b["body"].get("n")
            keep["kept"] = (b["body"].get("base") or {}).get("kept")
            keep["dropped"] = b["body"].get("dropped")
        rows.append({"status": status, "reason": reason, "kind": env["kind"], "seq": env.get("seq"), **keep})
    (run.dir / "envelopes-meta.json").write_text(json.dumps(rows, indent=1))


def fake_pdf():
    body = b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj<</Type/Catalog>>endobj\n" + bytes(range(256)) * 8 + b"\n%%EOF\n"
    return body


def auth_checks(run):
    base = run.base
    for _ in range(2):
        H.check(H.request(base, H.STATUS, key=run.c.management_key)[0] == 200, "valid status")
        for key in (None, "ca-wrong-key"):
            code, _, raw = H.request(base, H.STATUS, key=key)
            H.check(code in (401, 403), "unauthenticated status", code)
            run.mgmt_outputs.append(raw)
    H.check(H.request(base, H.STATUS, key=run.c.management_key)[0] == 200, "valid status after negatives")
    code, headers, shell = H.request(base, H.PUBLIC)
    H.check(code == 200 and shell.startswith(b"<!doctype html>"), "public shell", code)
    http_contract.assert_public_headers(headers, shell)
    for key in (None, run.c.management_key, "ca-wrong-key"):
        code2, _, again = H.request(base, H.PUBLIC + "?ignored=1", key=key)
        H.check(code2 == 200 and again == shell, "public shell changed with key/query")
    for path in ("/v0/resource/plugins/chat-archive/other", "/v0/resource/chat-archive/status"):
        H.check(H.request(base, path)[0] == 404, "public path", path)
    run.mgmt_outputs.append(shell)
    run.shell = shell
    code, _, raw = H.request(base, H.REQUEUE + "?reason=mask_violation&remask=1", key=run.c.management_key, method="POST", raw_body=b"")
    H.check(code == 202 and b'"job"' in raw, "requeue answers 202", code)
    run.mgmt_outputs.append(raw)
    code, _, raw = H.request(base, H.REQUEUE + "?reason=mask_violation&full=1", key=run.c.management_key, method="POST", raw_body=b"")
    H.check(code in (400, 409), "requeue full=1", code)
    return "shell %d bytes, CSP hashes match served bytes" % len(shell)


def model_lists(run):
    before = run.status()["counters"].get("ignored_model_list", 0)
    n_before = len(run.fake.get("/envelopes") or [])
    H.check(H.request(run.base, "/v1/models", headers={"Authorization": "Bearer " + run.c.client_key})[0] == 200, "/v1/models")
    H.check(H.request(run.base, "/v1beta/models", headers={"X-Goog-Api-Key": run.c.client_key})[0] == 200, "/v1beta/models")
    H.wait_until(lambda: run.status()["counters"].get("ignored_model_list", 0) >= before + 2, 10, "ignored_model_list counter")
    time.sleep(1.5)
    after = run.fake.get("/envelopes") or []
    new = [row for row in after[n_before:] if not row["envelope"]["body"].get("requested_model")]
    H.check(not new, "model-list GET produced envelopes", len(new))
    return "ignored_model_list=%d" % run.status()["counters"]["ignored_model_list"]


def midstream(run):
    code, raw = send_claude(run, "ca-claude-midstream", True)
    return "HTTP %d, %d bytes" % (code, len(raw))


def cancel_before(run):
    import http.client
    host, port = "127.0.0.1", run.config["server"]["port"]
    conn = http.client.HTTPConnection(host, port, timeout=30)
    raw = json.dumps(claude_body(run.c, "ca-claude-slow", True, text="cancel-before " + run.c.markers["request"])).encode()
    conn.request("POST", "/v1/messages", body=raw, headers={**H.client_headers(run.c, "x-api-key"), "Content-Type": "application/json"})
    time.sleep(0.8)  # the mock waits 3 s before the first byte
    conn.sock.shutdown(2)
    conn.close()
    return "closed after 0.8 s"


def cancel_after(run):
    import http.client
    host, port = "127.0.0.1", run.config["server"]["port"]
    conn = http.client.HTTPConnection(host, port, timeout=30)
    raw = json.dumps(claude_body(run.c, "ca-claude-slow", True, text="cancel-after " + run.c.markers["request"])).encode()
    conn.request("POST", "/v1/messages", body=raw, headers={**H.client_headers(run.c, "x-api-key"), "Content-Type": "application/json"})
    resp = conn.getresponse()
    H.check(resp.status == 200, "stream status", resp.status)
    first = resp.read1(65536)
    H.check(first, "no first chunk")
    conn.sock.shutdown(2)
    conn.close()
    return "first chunk %d bytes, then closed" % len(first)


def rejected(run):
    code, raw = send_claude(run, "reject-fixture", False, text="reject " + run.c.markers["request"])
    H.check(code == 403 and b"rejected_by_fixture" in raw, "reject fixture status", code)
    return "HTTP 403 from the test plugin"


def websocket(run, model, turns):
    port = run.config["server"]["port"]
    conn = wsproto.connect("127.0.0.1", port, "/v1/responses", {"Authorization": "Bearer " + run.c.client_key,
                                                                 "X-Archive-Machine": run.c.body["H8_archive_machine"],
                                                                 "Cookie": run.c.secret["H5_cookie"]})
    previous = None
    completed = 0
    try:
        for turn in range(turns):
            req = {"type": "response.create", "model": model, "instructions": "ca ws instructions",
                   "input": [{"type": "message", "role": "user", "content": [{"type": "input_text",
                              "text": "ws turn %d %s %s" % (turn, run.c.markers["request"], run.c.body["P9_github_pat"])}]}]}
            if previous:
                req["previous_response_id"] = previous
            conn.send_json(req)
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                event = conn.recv_json()
                if event.get("type") == "response.completed":
                    previous = event["response"]["id"]
                    completed += 1
                    break
                H.check(event.get("type") != "error", "websocket error event")
    finally:
        conn.close()
    H.check(completed == turns, model, "completed turns", completed)
    return "%d turns completed" % completed


def media_image(run):
    code, _, raw = H.request(run.base, "/v1/images/generations", body={"model": "grok-imagine-image", "prompt": "ca image " + run.c.markers["media"], "n": 1},
                             headers=H.client_headers(run.c), timeout=60)
    H.check(code == 200, "images", code, len(raw))
    return "HTTP 200"


def media_video(run):
    code, _, raw = H.request(run.base, "/v1/videos", body={"model": "grok-imagine-video", "prompt": "ca video " + run.c.markers["media"]},
                             headers=H.client_headers(run.c), timeout=60)
    H.check(code == 200, "video create", code)
    rid = json.loads(raw).get("request_id") or json.loads(raw).get("id")
    H.check(rid, "video id")
    code, _, raw = H.request(run.base, "/v1/videos/" + rid, headers=H.client_headers(run.c), method="GET", timeout=60)
    H.check(code == 200, "video poll", code)
    return "create+poll HTTP 200"


def media_speech(run):
    code, headers, raw = H.request(run.base, "/v1/audio/speech", body={"model": "grok-tts", "input": "ca speech " + run.c.markers["media"], "voice": "alloy"},
                                   headers=H.client_headers(run.c), timeout=60)
    H.check(code == 200 and len(raw) > 1000, "speech", code, len(raw))
    return "audio %d bytes" % len(raw)


# ---------------------------------------------------------------------------------------------
# Verification of captured envelopes


def verify_cases(run, view, c):
    fake = run.fake
    status = run.status()

    def fmt_case(model, stream, label):
        def check():
            begin, responses, comp, attempts = exchange(view, model, stream)
            r = responses[0]
            names = {h["name"] for h in begin["headers"]}
            for banned in ("authorization", "x-api-key", "x-goog-api-key", "proxy-authorization", "cookie", "x-custom-token"):
                H.check(banned not in names, label, "kept header", banned)
            H.check(begin["hints"].get("origin") == "native-acceptance", label, "hint origin")
            H.check("[masked:" in begin["hints"].get("machine", ""), label, "hint machine not masked")
            rnames = {h["name"] for h in r["headers"]}
            H.check("set-cookie" not in rnames and "x-api-key" not in rnames, label, "response kept set-cookie/x-api-key")
            if label == "claude":
                H.check("anthropic-ratelimit-unified-status" in rnames, label, "U7 positive control: allowed rate-limit header dropped")
            if stream:
                H.check(r.get("terminal_status") in ("completed", "done"), label, "terminal", r.get("terminal_status"))
            else:
                # CPA calls the response hook only after a successful non-stream execution.
                H.check(r.get("terminal_status") == "completed", label, "non-stream terminal", r.get("terminal_status"))
            text = piece_text(fake, refs_of(next(e for e in view.kind("exchange.response") if e["body"]["request_id"] == begin["request_id"])))
            H.check(c.markers["response"].encode() in text or c.markers["websocket"].encode() in text, label, "response marker missing from response pieces")
            return "items=%d responses=%d attempts=%d" % (begin["body"]["n"], len(responses), len(attempts))
        return check

    for model, label in (("ca-claude", "claude"), ("ca-openai", "openai"), ("ca-codex", "openai-response"), ("ca-gemini", "gemini")):
        for stream in (False, True):
            run_case(f"{label} {'stream' if stream else 'non-stream'}: begin/attempt/response/completion, headers, hints", fmt_case(model, stream, label))

    def thinking():
        found = {}
        for name in ("S1_thinking_signature", "S1_signature_delta", "S1_redacted_thinking", "S2_encrypted_content",
                     "S2_thought_signature", "S2_reasoning_encrypted", "S2_interactions_signature",
                     "S2_interactions_signature_delta"):
            digest = hashlib.sha256(c.secret[name].encode()).hexdigest()
            hits = fake.scan({"d": digest})["d"]
            found[name] = hits["bodies"] + hits["pieces"]
        missing = [k for k, v in found.items() if not v]
        H.check(not missing, "opaque stub sha256 missing for", missing)
        hits = fake.scan({"t": c.markers["thinking"]})["t"]
        H.check(hits["pieces"] + hits["bodies"], "thinking text missing")
        return "opaque sha256 present for %d signature fields; thinking text kept (Claude, OpenAI chat, Responses, Gemini, Interactions)" % len(found)
    run_case("thinking fixtures per format: signatures dropped, sha256 and len kept", thinking)

    def interactions():
        out = []
        for stream in (False, True):
            begin, responses, comp, attempts = exchange(view, "ca-interactions", stream)
            H.check(begin["source_format"] == "interactions", "source_format", begin["source_format"])
            r = responses[0]
            H.check(r["body"].get("format") == "interactions", "response format", r["body"].get("format"))
            env = next(e for e in view.kind("exchange.response") if e["body"]["request_id"] == begin["request_id"])
            text = piece_text(fake, refs_of(env))
            H.check(c.markers["thinking"].encode() in text and c.markers["response"].encode() in text, "interactions", "stream" if stream else "non-stream", "thinking or response text missing")
            out.append("%s items=%d" % ("stream" if stream else "non-stream", begin["body"]["n"]))
        return "; ".join(out)
    run_case("Interactions stream and non-stream: begin, response, thought summary and output kept", interactions)

    def codex_native():
        begin, responses, comp, _ = exchange(view, "ca-codex-native", True)
        env = next(e for e in view.kind("exchange.response") if e["body"]["request_id"] == begin["request_id"])
        text = piece_text(fake, refs_of(env))
        H.check(c.markers["codex_native"].encode() in text, "native Codex output not rebuilt from output_item.done")
        return "output rebuilt, %d response(s)" % len(responses)
    run_case("native Codex fixture: empty completed.output rebuilt from output_item.done", codex_native)

    def failover():
        begin, responses, comp, attempts = exchange(view, "ca-claude-failover", False)
        H.check(len(attempts) >= 2, "attempts", len(attempts))
        H.check(len({a["auth_fp"] for a in attempts}) >= 2, "attempt auth fingerprints not distinct")
        usage = [e["body"] for e in view.kind("usage.attempt") if e["body"].get("model") == "ca-claude-failover"]
        H.check(len(usage) >= 2, "usage records", len(usage))
        failed = [u for u in usage if u.get("failed")]
        H.check(failed and failed[0].get("failure_status") == 500 and failed[0].get("failure_body"), "failed usage with masked failure body")
        H.check(c.markers["failure"].encode() in piece_text(fake, [failed[0]["failure_body"]]), "failure marker missing from Failure.Body piece")
        return "attempts=%d usage=%d (one failed 500)" % (len(attempts), len(usage))
    run_case("failed upstream 500 then a second auth", failover)

    def u11():
        usage = [e["body"] for e in view.kind("usage.attempt") if e["body"].get("provider") == "claude"]
        fps = {u.get("account_fp") for u in usage if u.get("account_fp")}
        labels = {u.get("account_label") for u in usage if u.get("account_label")}
        H.check(len(fps) >= 2 and len(labels) >= 2, "claude keys fps/labels", len(fps), len(labels))
        H.check(all(u.get("source_kind") == "hashed" and not u.get("source_clear") for u in usage), "U2: apikey Source not hashed")
        return "fps=%d labels=%d, Source hashed" % (len(fps), len(labels))
    run_case("U11 two Claude API keys -> two labels and two fingerprints; U2 Source hashed", u11)

    def u1():
        scope1, scope2 = canarylib.caller_scope(c.client_key), canarylib.caller_scope(c.client_key_2)
        begins = view.kind("exchange.begin")
        H.check(begins and all(b["body"].get("caller_fp") in (scope1, scope2) for b in begins), "begin caller_fp")
        usage = view.kind("usage.attempt")
        H.check(usage and all(u["body"].get("caller_fp") in (scope1, scope2, "") for u in usage), "usage caller_fp")
        H.check(any(u["body"].get("caller_fp") for u in usage), "no usage caller_fp")
        return "caller_fp = CallerScope(APIKey) on %d usage records" % len(usage)
    run_case("U1 APIKey only as caller_fp equal to the hook CallerScope", u1)

    def u3_u10():
        usage = [e["body"] for e in view.kind("usage.attempt")]
        vertex = [u for u in usage if u.get("model") == "ca-vertex"]
        H.check(vertex and all(u.get("source_kind") == "hashed" for u in vertex), "U3 vertex Source not hashed")
        info = [u for u in usage if u.get("model") == "ca-openai-userinfo"]
        H.check(info, "U10 no usage")
        bu = info[0].get("base_url", "")
        H.check(bu.startswith("http://127.0.0.1:") and "@" not in bu and "?" not in bu, "U10 base_url not stripped", bu[:40])
        return "vertex hashed; base_url scheme://host/path"
    run_case("U3 vertex Source hashed; U10 BaseURL stripped", u3_u10)

    def mid():
        comps = view.completions_for("ca-claude-midstream", True)
        H.check(comps and comps[-1]["body"]["outcome"] == "failed", "mid-stream outcome", comps and comps[-1]["body"]["outcome"])
        return "outcome failed, error masked"
    run_case("mid-stream error -> completion failed", mid)

    def canceled():
        comps = view.completions_for("ca-claude-slow", True)
        outcomes = sorted(x["body"]["outcome"] for x in comps)
        H.check(outcomes.count("canceled") >= 2, "cancel outcomes", outcomes)
        return "outcomes=" + ",".join(outcomes)
    run_case("canceled before and after the first chunk", canceled)

    def bootstrap():
        # The first upstream attempt sends a cut-short data line and then fails; CPA holds the line
        # back, retries the stream and calls header-init again for the same request. The plugin
        # must drop what the first attempt delivered and keep only the second attempt's response.
        upstream = run.mock.attempts.get("ca-codex-bootstrap", 0)
        H.check(upstream == 2, "upstream attempts", upstream, "want 2 (CPA bootstrap retry)")
        begins = view.begins("ca-codex-bootstrap", True)
        H.check(begins, "no bootstrap begin")
        rid = begins[-1]["body"]["request_id"]
        resp = view.of("exchange.response", rid)
        H.check(len(resp) == 1, "responses", len(resp))
        body = resp[0]["body"]
        H.check(body.get("bootstrap_resets", 0) == 1, "bootstrap_resets", body.get("bootstrap_resets"))
        H.check(body.get("terminal_status") == "completed" and not body.get("truncated"), "response",
                body.get("terminal_status"), "truncated" if body.get("truncated") else "")
        comps = view.of("exchange.completion", rid)
        H.check(comps and comps[-1]["body"]["outcome"] == "succeeded", "outcome", comps and comps[-1]["body"]["outcome"])
        return "upstream attempts=2, bootstrap_resets=1, one completed response, outcome succeeded"
    run_case("bootstrap retry (two header-inits)", bootstrap)

    def reject():
        comps = view.completions_for("reject-fixture")
        H.check(comps and comps[-1]["body"]["outcome"] == "rejected", "reject outcome", comps and comps[-1]["body"]["outcome"])
        H.check(not comps[-1]["body"]["begin_written"], "rejected begin_written")
        return "outcome rejected, begin_written=false"
    run_case("rejected by a higher-priority test plugin", reject)

    def ws(model, label):
        def check():
            begins = view.begins(model)
            H.check(len(begins) >= 2, label, "begins", len(begins))
            H.check(all(b["body"].get("transport") == "ws" and b["body"].get("exec_session_id") for b in begins), label, "transport/exec_session_id")
            responses = [r for b in begins for r in view.of("exchange.response", b["body"]["request_id"])]
            H.check(len(responses) >= 2, label, "responses", len(responses))
            # The client closes the socket right after the last response.completed; that turn is
            # still succeeded, not canceled (CPA may cancel it while it reads the upstream close).
            outcomes = [c["body"]["outcome"] for b in begins for c in view.of("exchange.completion", b["body"]["request_id"])]
            H.check(len(outcomes) == len(begins) and set(outcomes) == {"succeeded"}, label, "completion outcomes", outcomes)
            based = [b for b in begins if b["body"]["body"].get("base")]
            return "begins=%d responses=%d completions succeeded=%d prefix-delta bases=%d" % (len(begins), len(responses), len(outcomes), len(based))
        return check
    run_case("Responses WebSocket merged", ws("ca-codex-merged", "merged"))
    run_case("Responses WebSocket native", ws("ca-codex-ws", "native"))

    def media():
        kinds = {b["body"].get("kind") for b in view.kind("exchange.begin")}
        for k in ("image", "video_create", "video_poll", "speech"):
            H.check(k in kinds, "media kind missing", k)
        fetches = run.fake.get("/media") or []
        H.check(fetches, "no POST /v1/media-fetch reached the fake archive")
        stripped = [f for f in fetches if "?" not in f["masked_url"] and f["masked_url"].startswith("https://")]
        H.check(stripped, "masked_url not stripped")
        hits = run.fake.scan({"u": stripped[0]["masked_url"]})["u"]
        H.check(hits["bodies"] + hits["pieces"] > 0, "stripped URL not in stored response")
        sig = run.fake.scan({"s": c.secret["M1_url_signature"]})["s"]
        H.check(sig["media_bodies"] > 0 and sig["bodies"] == 0 and sig["pieces"] == 0 and sig["heartbeats"] == 0, "M1 surfaces", sig)
        pdf = [b for b in view.kind("exchange.begin") if b["body"].get("requested_model") == "ca-claude" and b"application/pdf" in H.body_text(b)]
        H.check(pdf, "PDF attachment stub missing")
        return "image, video create/poll (%d media fetch), speech, PDF stub" % len(fetches)
    run_case("media: image, video (media-fetch + stripped URL), PDF, speech", media)

    def counters():
        st = run.status()
        dropped = {k: v for k, v in st["dropped"].items() if v}
        H.check(not st["recovered_panics"], "recovered panics", st["recovered_panics"])
        allowed = {"stopped"}
        H.check(set(dropped) <= allowed, "unexpected drops", dropped)
        return "dropped=%s faults=%s" % (dropped or "{}", st["faults"] or "[]")
    run_case("no drops and no recovered panics during the capture run", counters)


def scan_uploads(run, c, rules):
    secrets = c.all_secrets()
    allowed_media = {"M1_url_signature"}
    hits = run.fake.scan(secrets)
    leaks = {k: v for k, v in hits.items() if v["bodies"] or v["pieces"] or v["heartbeats"] or (v["media_bodies"] and k not in allowed_media)}
    H.check(not leaks, "secret found:", {k: {**v} for k, v in leaks.items()})
    markers = run.fake.scan({k: v for k, v in c.markers.items() if k != "media"})
    missing = [k for k, v in markers.items() if not (v["bodies"] or v["pieces"])]
    H.check(not missing, "ordinary markers missing from uploads:", missing)
    placeholders = {"[masked:" + r: "[masked:" + r for r in rules}
    ph = run.fake.scan(placeholders)
    absent = [k for k, v in ph.items() if not (v["bodies"] or v["pieces"])]
    H.check(not absent, "placeholders missing:", absent)
    return "%d secrets x encodings absent; %d markers and %d placeholder rule ids present" % (len(secrets), len(markers), len(rules))


def scan_outbox(run, c):
    data = run.data_dir()
    raw, decoded, modes, integrity = H.decoded_outbox(data, run.fake)
    H.check(integrity == "ok", "integrity_check", integrity)
    bad = [(p, oct(m)) for p, m, d in modes if (d and m != 0o700) or (not d and m != 0o600)]
    H.check(not bad, "modes", bad[:5])
    leaks = {}
    for name, value in c.all_secrets().items():
        found = canarylib.contains_any(raw, value) + canarylib.contains_any(decoded, value)
        if found:
            leaks[name] = sorted(set(found))
    H.check(not leaks, "secret in outbox:", leaks)
    missing = [k for k, v in c.markers.items() if k != "media" and v.encode() not in decoded]
    H.check(not missing, "markers missing from decoded outbox:", missing)
    return "%d files, %d decoded bytes; integrity ok; modes 0700/0600" % (sum(1 for _, _, d in modes if not d), len(decoded))


def scan_outbox_live(run, c):
    """Copy the data directory while CPA still runs and scan the copy raw and decoded (the copy is
    opened with sqlite3, so WAL frames count). Close checkpoints and truncates the WAL, so the
    scan after the stop never sees -wal content; the live copy covers that surface."""
    snap = None
    for _ in range(3):
        snap = Path(tempfile.mkdtemp(prefix="ca-live-outbox-", dir="/tmp"))
        shutil.copytree(run.data_dir(), snap / "data", symlinks=True)
        wal = snap / "data" / "outbox.sqlite-wal"
        if wal.exists() and wal.stat().st_size > 0:
            break
        shutil.rmtree(snap)
        snap = None
        # A maintenance checkpoint truncated the WAL just now: write something and copy again.
        send_claude(run, "ca-claude", False, text="wal " + c.markers["request"])
        time.sleep(1)
    H.check(snap is not None, "-wal empty in three live copies")
    try:
        wal_size = (snap / "data" / "outbox.sqlite-wal").stat().st_size
        raw, decoded, _, _ = H.decoded_outbox(snap / "data", run.fake)
        leaks = {}
        for name, value in c.all_secrets().items():
            found = canarylib.contains_any(raw, value) + canarylib.contains_any(decoded, value)
            if found:
                leaks[name] = sorted(set(found))
        H.check(not leaks, "secret in the live outbox copy:", leaks)
        missing = [k for k, v in c.markers.items() if k != "media" and v.encode() not in decoded]
        H.check(not missing, "markers missing from the decoded live copy:", missing)
        return "-wal %d bytes; %d raw and %d decoded bytes scanned" % (wal_size, len(raw), len(decoded))
    finally:
        shutil.rmtree(snap, ignore_errors=True)


CPA_OWN_LOG = set()


def cpa_own_logging(args, c, mock_state, mock_url):
    """Which canaries CPA v8.0.60 itself writes to its log with no plugin loaded (upstream error
    text it logs on its own). Those are attributed to CPA, not to the plugin."""
    run = Run(args, c, mock_state, mock_url, "cpa-own-log", args.library, plugins=False)
    try:
        run.start(wait_plugin=False)
        send_claude(run, "ca-claude-midstream", True)
        send_claude(run, "ca-claude-failover", False)
        send_claude(run, "ca-claude", True)
        send_openai(run, "ca-openai", False)
        run.cpa.stop()
        logs = run.cpa.logs()
    finally:
        run.close()
    found = {name for name, value in c.all_secrets().items() if canarylib.contains_any(logs, value)}
    found |= {"marker:" + name for name, value in c.markers.items() if value.encode() in logs}
    CPA_OWN_LOG.update(found)
    return "CPA without the plugin logs %d canaries itself: %s" % (len(found), ",".join(sorted(found)) or "none")


def scan_logs_mgmt(run, c):
    logs = run.cpa.logs()
    out = []
    own = []
    for label, blob in (("cpa log", logs), ("management", b"\n".join(run.mgmt_outputs))):
        for name, value in c.all_secrets().items():
            found = canarylib.contains_any(blob, value)
            if found and label == "cpa log" and name in CPA_OWN_LOG:
                own.append(name)
            elif found:
                out.append((label, name, found))
        for name, value in c.markers.items():
            if value.encode() in blob and label == "cpa log" and "marker:" + name in CPA_OWN_LOG:
                own.append("marker:" + name)
            elif value.encode() in blob:
                out.append((label, "marker:" + name, ["raw"]))
    H.check(not out, "found:", out)
    return "log %d bytes, %d management bodies%s" % (len(logs), len(run.mgmt_outputs),
        ("; CPA's own upstream-error logging (also without the plugin): " + ",".join(sorted(set(own)))) if own else "")


def reload_case(run):
    import http.client
    port = run.config["server"]["port"]
    run.mock.release.clear()
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=90)
    raw = json.dumps(claude_body(run.c, "ca-claude-hold", True, text="reload " + run.c.markers["request"])).encode()
    conn.request("POST", "/v1/messages", body=raw, headers={**H.client_headers(run.c, "x-api-key"), "Content-Type": "application/json"})
    resp = conn.getresponse()
    H.check(resp.status == 200, "hold stream", resp.status)
    first = resp.read1(65536)
    H.check(first, "no first chunk before reload")
    shutil.copy2(run.args.reload_library, run.dir / "plugins" / "chat-archive-v9.9.9.so")
    cfg = json.loads((run.dir / "config.yaml").read_text())
    cfg["routing"]["retry"]["max-retry-interval"] = 1  # any saved change makes CPA re-apply plugins
    (run.dir / "config.yaml").write_text(json.dumps(cfg))
    H.wait_until(lambda: run.status().get("version") == "9.9.9", 60, "new version registered")
    run.mock.release.set()
    rest = resp.read()
    conn.close()
    H.check(b"message_stop" in first + rest, "held stream did not finish")
    st = run.status()
    H.check(not st["recovered_panics"], "panics after reload", st["recovered_panics"])
    H.check(run.cpa.proc.poll() is None, "CPA died during reload")
    code, raw = send_claude(run, "ca-claude", False, text="after reload " + run.c.markers["request"])
    H.check(code == 200, "request after reload", code)
    return "stream finished across the reload; new instance 9.9.9 serving; no panic"


def restart_case(run, before):
    stats0 = run.fake.get("/stats")
    run.cpa.start()
    code, raw = send_claude(run, "ca-claude", False, text="after restart " + run.c.markers["request"])
    H.check(code == 200, "request after restart", code)
    settle(run)
    stats = run.fake.get("/stats")
    H.check(stats["keys_received_twice"] == 0 and stats["by_status"].get("duplicate", 0) == 0, "duplicates after restart", stats["keys_received_twice"], stats["by_status"])
    H.check(stats["keys"] > stats0["keys"], "nothing new after restart")
    code = run.cpa.stop(check_exit=False)
    H.check(code == 0, "second exit", code)
    return "envelopes %d -> %d, no key received twice" % (stats0["keys"], stats["keys"])


def phase_duplex(args, c, mock_state, mock_url):
    run = Run(args, c, mock_state, mock_url, "duplex", args.library, steering=True)
    try:
        run.start()
        H.wait_until(lambda: run.status().get("uploader", {}).get("archive_id"), 30, "sync")
        detail = websocket(run, "ca-codex-ws", 3)
        settle(run)
        view = View(run.fake)
        begins = view.begins("ca-codex-ws")
        by_rid = {}
        for b in begins:
            by_rid.setdefault(b["body"]["request_id"], []).append(b["body"].get("input_seq", 0))
        dump_meta(run)
        run.cpa.stop()
        H.check(begins, "no duplex begins")
        multi = {rid: seqs for rid, seqs in by_rid.items() if len(seqs) > 1}
        H.check(multi, "no RequestID carried duplex follow-ups (input_seq)", {k[:8]: v for k, v in by_rid.items()})
        for seqs in multi.values():
            H.check(sorted(seqs) == list(range(len(seqs))), "input_seq not in call order", seqs)
        # A duplex RequestID ends only with the socket, so CPA always reports the client's close
        # after the last response.completed as canceled; every response completed, so the
        # completion records succeeded.
        for rid in multi:
            terminals = [r["body"].get("terminal_status") for r in view.of("exchange.response", rid)]
            outcomes = [c["body"]["outcome"] for c in view.of("exchange.completion", rid)]
            H.check(terminals and set(terminals) == {"completed"}, "duplex terminal statuses", terminals)
            H.check(outcomes == ["succeeded"], "duplex completion outcome", outcomes)
        return detail + "; input_seq per RequestID " + str(sorted(multi.values())) + "; completion succeeded"
    finally:
        run.close()


# ---------------------------------------------------------------------------------------------
# Phase: nativefixture library (sanitized hook trace)


def phase_fixture(args, c, mock_state, mock_url):
    run = Run(args, c, mock_state, mock_url, "fixture", args.fixture_library)
    try:
        run.start()
        for stream in (False, True):
            code, _ = send_claude(run, "ca-claude", stream)
            H.check(code == 200, "fixture send", code)
        code, _ = send_openai(run, "ca-openai", True)

        def records():
            st = run.status()
            return st if st.get("fixture_records") else None
        st = H.wait_until(records, 20, "fixture records")
        raw = json.dumps(st).encode()
        leaks = [n for n, v in c.all_secrets().items() if canarylib.contains_any(raw, v)]
        marks = [n for n, v in c.markers.items() if v.encode() in raw]
        H.check(not leaks and not marks, "fixture status leaks", leaks, marks)
        hooks = sorted({r["hook"] for r in st["fixture_records"]})
        H.check("before_auth" in hooks and "stream_chunk" in hooks, "fixture hooks", hooks)
        run.cpa.stop()
        logs = run.cpa.logs()
        leaks = [n for n, v in c.all_secrets().items() if canarylib.contains_any(logs, v)]
        H.check(not leaks, "fixture CPA log leaks", leaks)
        return "%d records, hooks=%s" % (len(st["fixture_records"]), ",".join(hooks))
    finally:
        run.close()


# ---------------------------------------------------------------------------------------------
# Phase: memory and large prompts


def big_text(c, size):
    unit = ("ca large prompt filler " + c.markers["request"] + " ").encode()
    return (unit * (size // len(unit) + 1))[:size].decode()


def phase_memory(args, c, mock_state, mock_url):
    run = Run(args, c, mock_state, mock_url, "memory", args.library)
    peak = {"sys": 0, "status_polls": 0}
    stop = threading.Event()

    def poll():
        while not stop.is_set():
            try:
                st = run.status()
                run.mgmt_outputs.pop()
                peak["sys"] = max(peak["sys"], st.get("runtime", {}).get("sys", 0))
                peak["status_polls"] += 1
            except Exception:
                pass
            time.sleep(0.25)
    try:
        run.start()
        H.wait_until(lambda: run.status().get("uploader", {}).get("archive_id"), 30, "sync")
        poller = threading.Thread(target=poll, daemon=True)
        poller.start()
        results = {}

        def prompt(size, image=False):
            if image:
                img = b"\x89PNG\r\n\x1a\n" + bytes(range(256)) * (size // 256)
                extra = [{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": base64.b64encode(img[: size * 3 // 4]).decode()}}]
                return send_claude(run, "ca-claude-big", False, text="image-heavy " + c.markers["request"], extra_content=extra)
            return send_claude(run, "ca-claude-big", False, text=big_text(c, size))

        def single(label, size, image=False):
            def check():
                before = run.status()["dropped"]
                code, _ = prompt(size, image)
                H.check(code == 200, label, code)
                settle(run, timeout=240)
                after = run.status()["dropped"]
                delta = {k: after[k] - before.get(k, 0) for k in after if after[k] != before.get(k, 0)}
                H.check(not delta, label, "drops", delta)
                return "no drops; peak runtime.sys so far %d MiB" % (peak["sys"] >> 20)
            return check
        run_case("10 MB text prompt, no drops", single("10MB text", 10 << 20))
        run_case("10 MB base64-image prompt, no drops", single("10MB image", 10 << 20, True))
        run_case("50 MB text prompt, no drops", single("50MB text", 50 << 20))
        run_case("50 MB base64-image prompt, no drops", single("50MB image", 50 << 20, True))

        def concurrent():
            before = run.status()["dropped"]
            threads, codes = [], []
            for i in range(8):
                threads.append(threading.Thread(target=lambda: codes.append(prompt(int(8.5 * (1 << 20)))[0])))
            threads.append(threading.Thread(target=lambda: codes.append(prompt(50 << 20)[0])))
            for t in threads:
                t.start()
            for t in threads:
                t.join(600)
            H.check(codes.count(200) == 9, "concurrent codes", codes)
            settle(run, timeout=300)
            after = run.status()["dropped"]
            delta = {k: after[k] - before.get(k, 0) for k in after if after[k] != before.get(k, 0)}
            H.check(set(delta) <= {"body_budget"}, "concurrent drops other than body_budget", delta)
            results["concurrent_body_budget"] = delta.get("body_budget", 0)
            return "body_budget=%d" % delta.get("body_budget", 0)
        run_case("memory: 8 concurrent 8.5 MB + one 50 MB (shortfall only as body_budget)", concurrent)

        def long_ws():
            port = run.config["server"]["port"]
            conn = wsproto.connect("127.0.0.1", port, "/v1/responses", {"Authorization": "Bearer " + c.client_key})
            previous, done = None, 0
            try:
                for turn in range(40):
                    req = {"type": "response.create", "model": "ca-codex-ws",
                           "input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": big_text(c, 256 << 10)}]}]}
                    if previous:
                        req["previous_response_id"] = previous
                    conn.send_json(req)
                    while True:
                        ev = conn.recv_json()
                        if ev.get("type") == "response.completed":
                            previous = ev["response"]["id"]
                            done += 1
                            break
                        H.check(ev.get("type") != "error", "ws error")
            finally:
                conn.close()
            settle(run, timeout=240)
            return "%d responses on one socket" % done
        run_case("memory: long multi-response Responses WebSocket stream", long_ws)
        stop.set()
        poller.join(5)
        hb = run.fake.get("/heartbeats") or []
        hb_peak = max([h["runtime"]["sys"] for h in hb] or [0])
        # The harness leaves memory-limit-mib unset, so this checks the shipped default. The bound
        # is the limit the plugin reports it applied, and the peak must stay strictly below it.
        limit_mib = run.status()["config"]["memory_limit_mib"]
        limit = limit_mib << 20
        record("memory: peak plugin runtime.sys under memory-limit-mib (default %d MiB)" % DEFAULT_MEMORY_LIMIT_MIB,
               limit_mib == DEFAULT_MEMORY_LIMIT_MIB and max(peak["sys"], hb_peak) < limit,
               "limit %d MiB; peak status %d MiB over %d polls, heartbeat peak %d MiB" % (limit_mib, peak["sys"] >> 20, peak["status_polls"], hb_peak >> 20))
        RESULTS[-1]["peak_sys_bytes"] = max(peak["sys"], hb_peak)
        run.cpa.stop()
    finally:
        stop.set()
        run.close()


# ---------------------------------------------------------------------------------------------
# Phase: per-chunk latency (writer saturated) versus a no-plugin baseline


def chunk_gaps(base, c, n=5):
    import http.client
    gaps = []
    host, port = base.split("//")[1].split(":")
    for _ in range(n):
        conn = http.client.HTTPConnection(host, int(port), timeout=60)
        raw = json.dumps(claude_body(c, "ca-claude-latency", True, text="latency " + c.markers["request"])).encode()
        conn.request("POST", "/v1/messages", body=raw, headers={**H.client_headers(c, "x-api-key"), "Content-Type": "application/json"})
        resp = conn.getresponse()
        last = time.perf_counter()
        while True:
            chunk = resp.read1(65536)
            if not chunk:
                break
            now = time.perf_counter()
            gaps.append(now - last)
            last = now
        conn.close()
    gaps.sort()
    return gaps


def pct(values, p):
    return values[min(len(values) - 1, int(len(values) * p))] if values else 0.0


# Time to first byte A/B: streaming requests with these prompt sizes, sent while the writer is
# saturated, with and without the plugin.
TTFB_SIZES = (("1 MB", 1 << 20), ("8.5 MB", int(8.5 * (1 << 20))))
TTFB_SAMPLES = 20


def ttfb(base, c, size, n=TTFB_SAMPLES):
    """Seconds from sending a streaming request with a size-byte prompt to its first response byte, n
    times, sorted. Each stream is read to its end so CPA sees no cancel."""
    import http.client
    host, port = base.split("//")[1].split(":")
    raw = json.dumps(claude_body(c, "ca-claude-latency", True, text=big_text(c, size))).encode()
    headers = {**H.client_headers(c, "x-api-key"), "Content-Type": "application/json"}
    out = []
    for _ in range(n):
        conn = http.client.HTTPConnection(host, int(port), timeout=120)
        start = time.perf_counter()
        conn.request("POST", "/v1/messages", body=raw, headers=headers)
        resp = conn.getresponse()
        first = resp.read1(65536)
        out.append(time.perf_counter() - start)
        H.check(resp.status == 200 and first, "ttfb request", size, resp.status)
        while resp.read1(65536):
            pass
        conn.close()
    out.sort()
    return out


def under_load(run, c, measure):
    """measure() while three 4 MiB requests keep the capture workers and writer busy."""
    load = threading.Event()

    def saturate():
        while not load.is_set():
            send_claude(run, "ca-claude-big", False, text=big_text(c, 4 << 20))
    workers = [threading.Thread(target=saturate, daemon=True) for _ in range(3)]
    for w in workers:
        w.start()
    time.sleep(2)
    try:
        return measure()
    finally:
        load.set()
        for w in workers:
            w.join(120)


def loaded_measurements(run, c):
    """Chunk gaps and the TTFB of each size, all under the same saturating load."""
    return under_load(run, c, lambda: (chunk_gaps(run.base, c), {label: ttfb(run.base, c, size) for label, size in TTFB_SIZES}))


def phase_latency(args, c, mock_state, mock_url):
    base_run = Run(args, c, mock_state, mock_url, "baseline", args.library, plugins=False)
    try:
        base_run.start(wait_plugin=False)
        idle_base = chunk_gaps(base_run.base, c)
        baseline, ttfb_base = loaded_measurements(base_run, c)
        base_run.cpa.stop()
    finally:
        base_run.close()
    run = Run(args, c, mock_state, mock_url, "latency", args.library)
    try:
        run.start()
        idle_plugin = chunk_gaps(run.base, c)
        loaded, ttfb_plugin = loaded_measurements(run, c)
        st = run.status()
        settle(run, timeout=600)
        hooks = sorted(sum(e["body"].get("hook_micros", {}).values()) for e in View(run.fake).completions_for("ca-claude-latency", True))
        run.cpa.stop()
    finally:
        run.close()
    ms = lambda v, p: pct(v, p) * 1e3
    detail = ("client chunk gap p50/p99 ms, idle: no plugin %.2f/%.2f, plugin %.2f/%.2f; same load (3 x 4 MiB bodies "
              "in flight): no plugin %.2f/%.2f, plugin with writer saturated %.2f/%.2f; plugin stream_chunk hook p99 %s us") % (
        ms(idle_base, .5), ms(idle_base, .99), ms(idle_plugin, .5), ms(idle_plugin, .99),
        ms(baseline, .5), ms(baseline, .99), ms(loaded, .5), ms(loaded, .99), st["hook_p99_us"].get("stream_chunk"))
    record("per-chunk latency with the writer saturated (recorded, compared with no-plugin baseline)", True, detail)
    RESULTS[-1].update({"idle_baseline_p99_ms": ms(idle_base, .99), "idle_plugin_p99_ms": ms(idle_plugin, .99),
                        "loaded_baseline_p99_ms": ms(baseline, .99), "loaded_plugin_p99_ms": ms(loaded, .99),
                        "hook_p99_us": st["hook_p99_us"]})
    parts, values = [], {}
    for label, _ in TTFB_SIZES:
        b, p = ttfb_base[label], ttfb_plugin[label]
        parts.append("%s: no plugin %.1f/%.1f, plugin %.1f/%.1f" % (label, ms(b, .5), ms(b, .99), ms(p, .5), ms(p, .99)))
        values[label] = {"baseline_p50_ms": ms(b, .5), "baseline_p99_ms": ms(b, .99), "plugin_p50_ms": ms(p, .5), "plugin_p99_ms": ms(p, .99)}
    H.check(len(hooks) >= 2 * TTFB_SAMPLES, "completions with hook_micros", len(hooks))
    detail = ("time to first byte p50/p99 ms over %d streaming requests each, writer saturated (3 x 4 MiB bodies in flight): %s; "
              "plugin hook_micros per completion (all sync hooks) p50/p99 %d/%d us over %d completions") % (
        TTFB_SAMPLES, "; ".join(parts), pct(hooks, .5), pct(hooks, .99), len(hooks))
    record("time to first byte, 1 MB and 8.5 MB prompts, writer saturated (recorded, A/B against no plugin)", True, detail)
    RESULTS[-1].update({"ttfb": values, "hook_micros_p50_us": pct(hooks, .5), "hook_micros_p99_us": pct(hooks, .99),
                        "hook_micros_completions": len(hooks)})


# ---------------------------------------------------------------------------------------------
# Phase: blackholed archive


# Every archive route the plugin calls: held, they model a stalled archive (requests hang until
# the plugin's own deadlines, like a listener that never answers).
ARCHIVE_ROUTES = ["POST /v1/heartbeat", "POST /v1/ingest/need", "POST /v1/ingest/commit", "GET /v1/sources/self",
                  "PUT /v1/pieces/", "POST /v1/media-fetch"]
HEARTBEAT_EVERY = 60  # the plugin's heartbeat interval (internal/uploader HeartbeatEvery)


def phase_blackhole(args, c, mock_state, mock_url):
    """An unreachable archive must not slow CPA down. Heartbeats first reach the fake archive; then
    every archive route is held for longer than one heartbeat interval while CPA serves requests:
    no heartbeat arrives, the outbox grows, and hook latency stays within the stated bound of the
    reachable baseline taken in the same run. After the release a heartbeat arrives again, which
    shows the plugin kept trying. An unroutable archive address is also served."""
    run = Run(args, c, mock_state, mock_url, "blackhole", args.library)
    try:
        run.start()
        H.wait_until(lambda: run.fake.get("/heartbeats"), 30, "first heartbeat at the fake archive")
        settle(run)

        def timed(n):
            out = []
            for i in range(n):
                t0 = time.perf_counter()
                code, _ = send_claude(run, "ca-claude", i % 2 == 1)
                out.append(time.perf_counter() - t0)
                H.check(code == 200, "request", code)
            return out
        base = sorted(timed(10))
        st0 = run.status()
        for route in ARCHIVE_ROUTES:
            run.fake.post("/hold", {"route": route})
        time.sleep(1)  # a heartbeat already past the hold check has been recorded by now
        hb0 = len(run.fake.get("/heartbeats"))
        start = time.monotonic()
        held = []
        while time.monotonic() - start < HEARTBEAT_EVERY + 10:
            held += timed(2)
            time.sleep(4)
        held.sort()
        st1 = run.status()
        hb1 = len(run.fake.get("/heartbeats"))
        pending = st1.get("outbox", {}).get("pending", 0)
        run.fake.post("/release", {})
        H.wait_until(lambda: len(run.fake.get("/heartbeats")) > hb1, HEARTBEAT_EVERY + 15, "a heartbeat after the release")
        run.cpa.stop()
        unroutable = Run(args, c, mock_state, mock_url, "unroutable", args.library, archive_url="http://10.255.255.1:8790")
        try:
            unroutable.start()
            code, _ = send_claude(unroutable, "ca-claude", False)
            H.check(code == 200, "unroutable request", code)
            unroutable.cpa.stop()
        finally:
            unroutable.close()
        b50, h50 = base[len(base) // 2], held[len(held) // 2]
        p50_0, p50_1 = st0["hook_p50_us"].get("before_auth", 0), st1["hook_p50_us"].get("before_auth", 0)
        p99_0, p99_1 = st0["hook_p99_us"].get("before_auth", 0), st1["hook_p99_us"].get("before_auth", 0)
        detail = ("%d requests ok while held %.0f s; heartbeats %d before, %d during, more after the release; outbox pending %d; "
                  "request p50 %.1f ms reachable vs %.1f ms held; before_auth hook p50 %s -> %s us (p99 %s -> %s us, recorded); "
                  "unroutable address also served") % (
            len(held), HEARTBEAT_EVERY + 10, hb0, hb1 - hb0, pending, b50 * 1e3, h50 * 1e3, p50_0, p50_1, p99_0, p99_1)
        # Bounds: a held archive adds no hook work, so the client median stays within 2x + 50 ms of
        # the reachable baseline and the cumulative before_auth p50 (log2 buckets) moves at most one
        # bucket. The p99 is recorded, not bounded: over about 50 calls it is the single slowest
        # call, which scheduling on a shared host dominates.
        ok = hb1 == hb0 and pending > 0 and h50 <= 2 * b50 + 0.05 and p50_1 <= 2 * max(p50_0, 1)
        record("blackhole: CPA keeps serving, heartbeats stop reaching the archive, outbox grows, hook latency unchanged", ok, detail)
    finally:
        run.close()


# ---------------------------------------------------------------------------------------------


def maskcheck(args, c):
    import subprocess
    values = list(c.body.values())
    q = {"values": values, "exact": [c.ops_password, c.hmac_key], "callers": [c.client_key]}
    out = json.loads(subprocess.run([str(args.fakearchive), "maskcheck"], input=json.dumps(q).encode(), capture_output=True, check=True).stdout)
    unmasked = [name for name, r in zip(c.body, out) if not r["changed"]]
    H.check(not unmasked, "harness canaries the masker does not catch:", unmasked)
    rules = sorted({rule for r in out for rule in r["rules"]})
    return rules


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("cpa", "library", "reload_library", "fixture_library", "reject_library", "fakearchive"):
        parser.add_argument("--" + name.replace("_", "-"), type=Path, required=True)
    parser.add_argument("--only", default="main,duplex,fixture,memory,latency,blackhole")
    parser.add_argument("--results", type=Path, default=Path("/tmp/chat-archive-native-results.json"))
    args = parser.parse_args()
    phases = set(args.only.split(","))
    c = canarylib.Canaries()
    rules = maskcheck(args, c)
    mock_state, server, thread = mockup.start(c)
    mock_url = "http://127.0.0.1:%d" % server.server_port
    try:
        if "main" in phases:
            run_case("CPA's own logging without the plugin (attribution baseline)", cpa_own_logging, args, c, mock_state, mock_url)
            run_case("phase main", phase_main, args, c, mock_state, mock_url, rules)
        if "duplex" in phases:
            run_case("Responses WebSocket duplex (upstream.codex.response-steering)", phase_duplex, args, c, mock_state, mock_url)
        if "fixture" in phases:
            run_case("nativefixture status holds hook names, counts and reason codes only", phase_fixture, args, c, mock_state, mock_url)
        if "memory" in phases:
            run_case("phase memory", phase_memory, args, c, mock_state, mock_url)
        if "latency" in phases:
            run_case("phase latency", phase_latency, args, c, mock_state, mock_url)
        if "blackhole" in phases:
            run_case("phase blackhole", phase_blackhole, args, c, mock_state, mock_url)
    finally:
        server.shutdown()
        server.server_close()
    unknown = sorted(set(mock_state.unknown))
    if unknown:
        print("mock upstream unknown routes: " + ", ".join(m + " " + p for m, p in unknown), flush=True)
    args.results.write_text(json.dumps({"results": RESULTS, "mock_requests": len(mock_state.seen), "unknown_routes": unknown}, indent=2))
    failed = [r for r in RESULTS if r["result"] == "FAIL"]
    print("SUMMARY: %d passed, %d failed" % (len(RESULTS) - len(failed), len(failed)), flush=True)
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
