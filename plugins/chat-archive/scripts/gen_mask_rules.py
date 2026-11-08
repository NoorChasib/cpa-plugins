#!/usr/bin/env python3
"""Generate pkg/mask/rules_gitleaks_v8_30_1.go from the vendored gitleaks config.

Python 3.11+ standard library only (tomllib, hashlib, urllib). The input is
pkg/mask/testdata/gitleaks-v8.30.1.toml, checked against the pinned sha256 before anything is
generated. The output is deterministic, gofmt-clean Go, so regenerating reproduces the committed file
byte for byte.

  gen_mask_rules.py           write the Go file
  gen_mask_rules.py --check   exit 1 if the committed Go file differs from a fresh generation
  gen_mask_rules.py --fetch   download config/gitleaks.toml at the pinned commit, verify its sha256,
                              store it as the vendored toml, then write the Go file

Semantics kept from gitleaks (detect/detect.go:520-562 at v8.30.1) are applied by pkg/mask. What the
generator leaves out, because chat content has no file paths or commits: path-scoped rules, and path and commit
allowlists. An allowlist with condition AND that has paths can never match without a file path, and
an allowlist whose only checks are paths or commits never matches a finding, so both are dropped
whole; the remaining allowlists keep their regexes, stopwords, regexTarget and condition.

One deliberate deviation from gitleaks (GLOBAL_REGEX_FIXES): the global allowlist regex
`(?i)^true|false|null$` has a precedence bug. It reads as `^true` OR `false` OR `null$`, so any secret
that starts with "true", contains "false" or ends with "null" would be allowlisted, which exempts
human-chosen passwords. It is written `(?i)^(?:true|false|null)$`, the evident intent. The output
header names every rewrite.
"""

import argparse
import hashlib
import json
import sys
import tomllib
import urllib.request
from pathlib import Path

GENERATOR_VERSION = "2"
TAG = "v8.30.1"
COMMIT = "83d9cd684c87d95d656c1458ef04895a7f1cbd8e"
TOML_SHA256 = "e163e53b9e7e8a8511e77271e2b323ed057759542a6d988258afe3a1fa329caf"
TOML_URL = f"https://raw.githubusercontent.com/gitleaks/gitleaks/{COMMIT}/config/gitleaks.toml"
EXPECTED_RULES = 217

MODULE_DIR = Path(__file__).resolve().parent.parent
TOML_PATH = MODULE_DIR / "pkg" / "mask" / "testdata" / "gitleaks-v8.30.1.toml"
OUT_PATH = MODULE_DIR / "pkg" / "mask" / "rules_gitleaks_v8_30_1.go"

# Global allowlist regexes rewritten on purpose (gitleaks pattern -> emitted pattern); see the module
# docstring.
GLOBAL_REGEX_FIXES = {
    r"(?i)^true|false|null$": r"(?i)^(?:true|false|null)$",
}

RULE_KEYS = {"id", "description", "regex", "keywords", "entropy", "allowlists", "path", "secretGroup"}
ALLOW_KEYS = {"description", "condition", "commits", "paths", "regexTarget", "regexes", "stopwords"}


def fail(msg):
    print(f"gen_mask_rules: {msg}", file=sys.stderr)
    sys.exit(1)


def verified(data):
    got = hashlib.sha256(data).hexdigest()
    if got != TOML_SHA256:
        fail(f"sha256 mismatch: got {got}, want {TOML_SHA256}")
    return data


def raw(s):
    """A Go raw string literal."""
    if "`" in s:
        fail("pattern contains a backtick; extend the generator before using this ruleset")
    return "`" + s + "`"


def quoted(s):
    """A Go interpreted string literal (inputs are printable ASCII; JSON escapes are valid Go)."""
    if not s.isascii() or not s.isprintable():
        fail(f"non-printable or non-ASCII string {s!r}")
    return json.dumps(s)


def number(v):
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        fail(f"bad number {v!r}")
    return repr(v)


def condition(a):
    c = str(a.get("condition", "")).upper()
    if c in ("", "OR", "||"):
        return "OR"
    if c in ("AND", "&&"):
        return "AND"
    fail(f"unknown allowlist condition {c!r}")


def kept_allowlist(a, where):
    """Returns the allowlist without path/commit checks, or None when it could never match."""
    unknown = set(a) - ALLOW_KEYS
    if unknown:
        fail(f"{where}: unknown allowlist keys {sorted(unknown)}")
    regexes = list(a.get("regexes", []))
    stopwords = list(a.get("stopwords", []))
    has_path_or_commit = bool(a.get("paths")) or bool(a.get("commits"))
    cond = condition(a)
    if cond == "AND" and has_path_or_commit:
        return None
    if not regexes and not stopwords:
        return None
    target = a.get("regexTarget", "")
    if target not in ("", "secret", "match", "line"):
        fail(f"{where}: unknown regexTarget {target!r}")
    if target == "secret":
        target = ""
    return {"target": target, "regexes": regexes, "stopwords": stopwords, "condition": cond}


class Emitter:
    def __init__(self):
        self.lines = []
        self.stopword_vars = []  # (name, words)

    def allow(self, a, owner):
        parts = []
        if a["target"]:
            parts.append(f"RegexTarget: {quoted(a['target'])}")
        if a["regexes"]:
            parts.append("Regexes: []string{" + ", ".join(raw(r) for r in a["regexes"]) + "}")
        if a["stopwords"]:
            if len(a["stopwords"]) > 8:
                name = f"stopWords{len(self.stopword_vars)}"
                self.stopword_vars.append((name, owner, a["stopwords"]))
                parts.append(f"StopWords: {name}")
            else:
                parts.append("StopWords: []string{" + ", ".join(quoted(w) for w in a["stopwords"]) + "}")
        if a["condition"] == "AND":
            parts.append('Condition: "AND"')
        return "{" + ", ".join(parts) + "}"


def generate(data):
    doc = tomllib.loads(data.decode("utf-8"))
    if set(doc) - {"title", "minVersion", "allowlist", "allowlists", "rules"}:
        fail(f"unknown top-level keys {sorted(set(doc) - {'title', 'minVersion', 'allowlist', 'allowlists', 'rules'})}")

    globals_ = []
    if "allowlist" in doc:
        globals_.append(doc["allowlist"])
    globals_.extend(doc.get("allowlists", []))

    em = Emitter()
    out = em.lines
    dropped_rules = []
    dropped_allow = []

    global_kept = []
    for i, a in enumerate(globals_):
        if a.get("targetRules"):
            fail("targeted global allowlists are not supported by this generator")
        k = kept_allowlist({x: y for x, y in a.items() if x != "targetRules"}, f"allowlist[{i}]")
        if k is None:
            dropped_allow.append(f"global allowlist {i}")
        else:
            k["regexes"] = [GLOBAL_REGEX_FIXES.get(r, r) for r in k["regexes"]]
            global_kept.append(k)
    fixed = [src for src in GLOBAL_REGEX_FIXES if any(src in a.get("regexes", []) for a in globals_)]
    if len(fixed) != len(GLOBAL_REGEX_FIXES):
        fail("a GLOBAL_REGEX_FIXES pattern is no longer in the global allowlist")

    rule_lines = []
    seen = set()
    for r in doc["rules"]:
        unknown = set(r) - RULE_KEYS
        if unknown:
            fail(f"{r.get('id')}: unknown rule keys {sorted(unknown)}")
        rid = r["id"]
        if rid in seen:
            fail(f"duplicate rule id {rid}")
        seen.add(rid)
        if "path" in r:
            dropped_rules.append(rid)
            continue
        if "regex" not in r:
            fail(f"{rid}: rule without regex or path")
        parts = [f"ID: {quoted(rid)}", f"Regex: {raw(r['regex'])}"]
        kws = [k.lower() for k in r.get("keywords", [])]
        if kws:
            parts.append("Keywords: []string{" + ", ".join(quoted(k) for k in kws) + "}")
        if "entropy" in r:
            parts.append(f"Entropy: {number(r['entropy'])}")
        if r.get("secretGroup"):
            parts.append(f"SecretGroup: {int(r['secretGroup'])}")
        allows = []
        for j, a in enumerate(r.get("allowlists", [])):
            k = kept_allowlist(a, f"{rid}.allowlists[{j}]")
            if k is None:
                dropped_allow.append(f"{rid} allowlist {j}")
            else:
                allows.append(em.allow(k, rid))
        if allows:
            parts.append("Allow: []allowSpec{" + ", ".join(allows) + "}")
        desc = " ".join(str(r.get("description", "")).split())
        rule_lines.append(f"\t// {desc}" if desc else None)
        rule_lines.append("\t{" + ", ".join(parts) + "},")

    n_rules = sum(1 for line in rule_lines if line and not line.startswith("\t//"))
    if n_rules != EXPECTED_RULES:
        fail(f"got {n_rules} non-path-scoped rules, want {EXPECTED_RULES}")

    out.append("// Code generated by scripts/gen_mask_rules.py; DO NOT EDIT.")
    out.append("")
    out.append(f"// Source: gitleaks {TAG} (commit {COMMIT}), config/gitleaks.toml,")
    out.append(f"// sha256 {TOML_SHA256} ({len(data)} bytes),")
    out.append("// vendored as pkg/mask/testdata/gitleaks-v8.30.1.toml.")
    out.append(f"// Generator: scripts/gen_mask_rules.py version {GENERATOR_VERSION}.")
    out.append("//")
    out.append("// gitleaks is MIT licensed, Copyright (c) 2019 Zachary Rice; the full license text is in")
    out.append("// pkg/mask/LICENSE-gitleaks.")
    out.append("//")
    out.append(f"// Dropped path-scoped rules: {', '.join(dropped_rules)}.")
    out.append("// Dropped allowlists (path or commit checks only, or AND with paths): " + ", ".join(dropped_allow) + ".")
    for src, dst in GLOBAL_REGEX_FIXES.items():
        out.append(f"// Deviation from gitleaks (precedence fix): global allowlist regex `{src}` is emitted as `{dst}`.")
    out.append("")
    out.append("package mask")
    out.append("")
    out.append(f"const gitleaksTag = {quoted(TAG)}")
    out.append("")
    out.append(f"const gitleaksCommit = {quoted(COMMIT)}")
    out.append("")
    out.append(f"const gitleaksTOMLSHA256 = {quoted(TOML_SHA256)}")
    out.append("")
    out.append(f"const genMaskRulesVersion = {quoted(GENERATOR_VERSION)}")
    out.append("")
    out.append("// gitleaksGlobalAllow is the global allowlist, checked before each rule's own allowlists.")
    out.append("var gitleaksGlobalAllow = []allowSpec{")
    for k in global_kept:
        out.append("\t" + em.allow(k, "global") + ",")
    out.append("}")
    out.append("")
    out.append(f"// gitleaksRules are the {EXPECTED_RULES} non-path-scoped rules, in config order.")
    out.append("var gitleaksRules = []ruleSpec{")
    for line in rule_lines:
        if line is not None:
            out.append(line)
    out.append("}")
    for name, owner, words in em.stopword_vars:
        out.append("")
        out.append(f"// {name} are the stopwords of a {owner} allowlist.")
        out.append(f"var {name} = []string{{")
        for w in words:
            out.append(f"\t{quoted(w)},")
        out.append("}")
    return ("\n".join(out) + "\n").encode("utf-8")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--check", action="store_true", help="compare with the committed file instead of writing")
    ap.add_argument("--fetch", action="store_true", help="download and verify the toml at the pinned commit first")
    args = ap.parse_args()

    if args.fetch:
        with urllib.request.urlopen(TOML_URL, timeout=60) as resp:
            data = verified(resp.read())
        TOML_PATH.parent.mkdir(parents=True, exist_ok=True)
        TOML_PATH.write_bytes(data)
    else:
        data = verified(TOML_PATH.read_bytes())

    src = generate(data)
    if args.check:
        current = OUT_PATH.read_bytes() if OUT_PATH.exists() else b""
        if current != src:
            fail(f"{OUT_PATH.relative_to(MODULE_DIR)} is stale; run scripts/gen_mask_rules.py")
        print(f"{OUT_PATH.relative_to(MODULE_DIR)} is up to date")
        return
    OUT_PATH.write_bytes(src)
    print(f"wrote {OUT_PATH.relative_to(MODULE_DIR)} ({len(src)} bytes)")


if __name__ == "__main__":
    main()
