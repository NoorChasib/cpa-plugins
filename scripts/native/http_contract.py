"""Assert security headers as received over actual HTTP, hashing served asset bytes."""
import base64
import hashlib
from html.parser import HTMLParser


class InlineAssets(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=False)
        self.assets = {"script": [], "style": []}
        self.active = None

    def handle_starttag(self, tag, attrs):
        if tag in self.assets:
            assert not attrs and self.active is None, "expected fixed inline script/style without attributes"
            self.active = tag
            self.assets[tag].append("")

    def handle_data(self, data):
        if self.active:
            self.assets[self.active][-1] += data

    def handle_endtag(self, tag):
        if tag == self.active:
            self.active = None


def expected_csp(raw):
    parser = InlineAssets()
    parser.feed(raw.decode("utf-8"))
    parser.close()
    assert parser.active is None
    hashes = {}
    for tag, assets in parser.assets.items():
        assert len(assets) == 1 and assets[0], "expected one nonempty fixed " + tag
        digest = base64.b64encode(hashlib.sha256(assets[0].encode("utf-8")).digest()).decode()
        hashes[tag + "-src"] = ["'sha256-" + digest + "'"]
    return {"default-src": ["'none'"], **hashes, "connect-src": ["'self'"],
            "frame-ancestors": ["'self'"], "base-uri": ["'none'"],
            "form-action": ["'none'"], "object-src": ["'none'"]}


def assert_public_headers(headers, raw):
    expected = {"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store",
                "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"}
    for name, value in expected.items():
        assert headers.get_all(name, []) == [value], (name, headers.get_all(name))
    values = headers.get_all("Content-Security-Policy", [])
    assert len(values) == 1, "exactly one enforced Content-Security-Policy is required"
    actual = {}
    for directive in values[0].split(";"):
        words = directive.split()
        if words:
            assert words[0] not in actual, "duplicate CSP directive"
            actual[words[0]] = words[1:]
    # Exact allowlist also rejects unsafe-inline/eval, wildcards, unexpected
    # script/style origins, missing directives, and hashes of different bytes.
    assert actual == expected_csp(raw), "CSP must exactly constrain the served fixed script/style bytes"
