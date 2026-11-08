"""Synthetic canaries for chat-archive native acceptance.

Each canary has a short id used only as a label in results: a letter for where it is planted
(H request or response header, U usage record or upstream credential, R response body, S
signature, M media URL, P a body secret one mask/v1 rule catches, E an exact-registered value)
and a number; the dict keys below and the comments beside each credential define them.

Every value is generated at test time from os.urandom; none resembles real data. Secret
canaries must be absent from every surface in every encoding; ordinary markers must be present
in captured data and absent from management output, logs and status. Only canary names, counts
and surface names are ever printed, never a value.
"""
import base64
import hashlib
import json
import os
import secrets
import string
import urllib.parse

ALNUM = string.ascii_letters + string.digits
UPPER_ALNUM = string.ascii_uppercase + string.digits
B64URLISH = ALNUM + "_-"


def rand(alphabet, n):
    return "".join(secrets.choice(alphabet) for _ in range(n))


def hexs(n):
    return secrets.token_hex(n // 2)


def caller_scope(key):
    return hashlib.sha256(b"cli-proxy-api:caller-scope:v1\x00" + key.strip().encode()).hexdigest()


def exact_fp(value):
    return hashlib.sha256(b"chat-archive/exact/v1\x00" + value.encode()).hexdigest()


def pem_block():
    lines = [rand(ALNUM + "+/", 64) for _ in range(12)]
    return "-----BEGIN RSA PRIVATE KEY-----\n" + "\n".join(lines) + "\n-----END RSA PRIVATE KEY-----"


def jwt():
    def part(obj):
        return base64.urlsafe_b64encode(json.dumps(obj).encode()).rstrip(b"=").decode()
    return part({"alg": "HS256", "typ": "JWT"}) + "." + part({"sub": rand(ALNUM, 24), "iat": 1700000000}) + "." + rand(B64URLISH, 43)


class Canaries:
    """One run's canaries. `secret` maps id -> value (must be absent); `body` maps id -> value
    planted in body text that mask/v1 must replace; `markers` maps id -> ordinary text."""

    def __init__(self):
        tag = hexs(8)
        # Credentials the harness configures. They reach the plugin through headers, Source,
        # APIKey and BaseURL, never through content unless planted below on purpose.
        self.client_key = "sk-cpa-" + rand(ALNUM, 40)          # H1, U1; registered caller (E3)
        self.client_key_2 = "sk-cpa-" + rand(ALNUM, 40)        # x-api-key client (H2)
        self.management_key = "ca-mgmt-" + rand(ALNUM, 32)
        self.claude_key_a = "sk-ant-api03-" + rand(B64URLISH, 93) + "AA"   # U2, U11
        self.claude_key_b = "sk-ant-api03-" + rand(B64URLISH, 93) + "AA"   # U11
        self.openai_key = "sk-proj-" + rand(ALNUM, 48)
        self.codex_key = "sk-proj-" + rand(ALNUM, 48)
        self.gemini_key = "AIza" + rand(B64URLISH, 35)                     # H3 shape, upstream
        self.vertex_key = "AIza" + rand(B64URLISH, 35)                     # U3
        self.xai_key = "xai-" + rand(ALNUM, 80)
        self.source_token = None  # filled from the fake archive (cha_src_...)
        self.hmac_key = "ca-hmac-" + rand(ALNUM, 48)                       # E4
        self.ops_password = rand(ALNUM, 64)                                # E2 (registered by fingerprint)
        self.secret = {
            "H4_proxy_authorization": "Basic " + rand(ALNUM, 40),
            "H5_cookie": "ca_session=" + rand(ALNUM, 40),
            "H5_set_cookie": "ca_resp_session=" + rand(ALNUM, 40),
            "H6_custom_token": "ca-custom-" + rand(ALNUM, 40),
            "H8_archive_foo": "ca-foo-" + rand(ALNUM, 40),
            "U7_echoed_api_key": "ca-echo-" + rand(ALNUM, 40),
            "U10_userinfo_password": "ca-userinfo-" + rand(ALNUM, 24),
            "U10_query_secret": "ca-query-" + rand(ALNUM, 24),
            "S1_thinking_signature": "EqQB" + rand(ALNUM + "+/", 120),
            "S1_signature_delta": "EqQC" + rand(ALNUM + "+/", 120),
            "S1_redacted_thinking": "EmwK" + rand(ALNUM + "+/", 120),
            "S2_encrypted_content": "gAAAAA" + rand(ALNUM + "+/", 120),
            "S2_thought_signature": "CsoB" + rand(ALNUM + "+/", 120),
            "S2_reasoning_encrypted": "ZW5j" + rand(ALNUM + "+/", 120),
            "S2_interactions_signature": "EtIB" + rand(ALNUM + "+/", 120),        # Interactions thought step
            "S2_interactions_signature_delta": "EtIC" + rand(ALNUM + "+/", 120),  # Interactions stream delta
            "M1_url_signature": hexs(64),
        }
        # Secrets planted inside body text, each written in a form a mask/v1 rule catches.
        self.body = {
            "P1_anthropic": "sk-ant-api03-" + rand(B64URLISH, 93) + "AA",
            "P3_openrouter": "sk-or-v1-" + hexs(64),
            "P4_openai_project": "sk-proj-" + rand(ALNUM, 48),
            "P5_stripe": "sk_live_" + rand(ALNUM, 32),
            "P6_xai": "xai-" + rand(ALNUM, 80),
            "P7_google": "AIza" + rand(B64URLISH, 35),
            "P9_github_pat": "ghp_" + rand(ALNUM, 36),
            "P10_github_oauth": "gho_" + rand(ALNUM, 36),
            "P14_github_fine": "github_pat_" + rand(ALNUM, 22) + "_" + rand(ALNUM, 59),
            "P15_gitlab": "glpat-" + rand(ALNUM, 20),
            "P16_huggingface": "hf_" + rand(string.ascii_letters, 34),
            "P17_npm": "npm_" + rand(ALNUM, 36),
            "P18_slack_bot": "xoxb-" + rand(string.digits, 11) + "-" + rand(string.digits, 12) + "-" + rand(ALNUM, 24),
            "P20_jwt": jwt(),
            "E1_archive_token": "cha_src_" + rand(ALNUM, 43),
            "R2_split_stream_secret": "ghp_" + rand(ALNUM, 36),
            "U8_failure_anthropic": "sk-ant-api03-" + rand(B64URLISH, 93) + "AA",
            "U8_failure_google": "AIza" + rand(B64URLISH, 35),
            "R3_completion_error": "ghp_" + rand(ALNUM, 36),
            "H7_prompt_id": "sk-ant-oat01-" + rand(B64URLISH, 93) + "AA",
            "H8_archive_machine": "ghp_" + rand(ALNUM, 36),
            "P19_private_key": pem_block(),
        }
        self.markers = {
            "request": "ordinary-request-marker-" + tag,
            "response": "ordinary-response-marker-" + tag,
            "thinking": "ordinary-thinking-marker-" + tag,
            "codex_native": "ordinary-codex-native-marker-" + tag,
            "websocket": "ordinary-websocket-marker-" + tag,
            "failure": "ordinary-failure-marker-" + tag,
            "media": "ordinary-media-marker-" + tag,
        }

    def configured_secrets(self):
        out = {"H1_U1_client_key": self.client_key, "H2_client_key_2": self.client_key_2,
               "management_key": self.management_key, "U2_U11_claude_key_a": self.claude_key_a,
               "U11_claude_key_b": self.claude_key_b, "openai_key": self.openai_key, "codex_key": self.codex_key,
               "gemini_key": self.gemini_key, "U3_vertex_key": self.vertex_key, "xai_key": self.xai_key,
               "E4_hmac_key": self.hmac_key, "E2_ops_password": self.ops_password}
        if self.source_token:
            out["source_token"] = self.source_token
        return out

    def all_secrets(self):
        return {**self.configured_secrets(), **self.secret, **self.body}

    def content_secrets_text(self):
        """Body text carrying every content canary plus the exact-value ones (E2, E3, E4)."""
        parts = [self.markers["request"]]
        for name, value in self.body.items():
            if name.startswith(("R2_", "U8_", "R3_", "H7_", "H8_")):
                continue
            parts.append(name + ": " + value)
        parts.append("E2 ops password: " + self.ops_password)
        parts.append("E3 caller key: " + self.client_key)
        parts.append("E4 hmac key: " + self.hmac_key)
        parts.append("E5 env line OPS_PASSWORD=" + self.ops_password)
        return "\n".join(parts)


def variants(value):
    """Encodings searched for each canary: raw, JSON-escaped, base64 standard
    and URL alphabets at each byte alignment, percent-encoded, lowercase and uppercase hex."""
    raw = value.encode()
    out = {"raw": raw}
    escaped = json.dumps(value)[1:-1].encode()
    if escaped != raw:
        out["json"] = escaped
    slash = escaped.replace(b"/", b"\\/")
    if slash != escaped:
        out["json_slash"] = slash
    for k in range(3):
        for name, enc in (("b64", base64.b64encode), ("b64url", base64.urlsafe_b64encode)):
            full = enc(b"\0" * k + raw).rstrip(b"=")
            start = 4 if k else 0
            stop = len(full) - ((len(full) - start) % 4 if (k + len(raw)) % 3 else 0)
            core = full[start:stop]
            if len(core) >= 12:
                out[f"{name}{k}"] = core
    pct = urllib.parse.quote(value, safe="").encode()
    if pct != raw:
        out["percent"] = pct
    out["hex"] = raw.hex().encode()
    out["HEX"] = raw.hex().upper().encode()
    return out


def contains_any(blob, value):
    """Names of the encodings of value found in blob."""
    return sorted(name for name, needle in variants(value).items() if needle in blob)


def needles(named):
    """Flatten {id: value} into {id/encoding: base64(needle)} for the fake archive /scan."""
    out = {}
    for name, value in named.items():
        for enc, needle in variants(value).items():
            out[name + "/" + enc] = base64.b64encode(needle).decode()
    return out
