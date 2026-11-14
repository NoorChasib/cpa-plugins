package capture

import (
	"net/http"
	"testing"
)

// TestHeaderList: repeated Content-Encoding field lines combine in order (RFC 9110 5.3), so a body
// gzipped and then br-encoded names both codings; the first line alone would leave it encoded.
func TestHeaderList(t *testing.T) {
	h := http.Header{"Content-Encoding": {"gzip", "br"}, "Content-Type": {"application/json"}}
	if got := headerList(h, "content-encoding"); got != "gzip, br" {
		t.Fatalf("%q", got)
	}
	if got := headerList(http.Header{"content-encoding": {"zstd"}}, "Content-Encoding"); got != "zstd" {
		t.Fatalf("lowercase key: %q", got)
	}
	if got := headerList(nil, "Content-Encoding"); got != "" {
		t.Fatalf("absent: %q", got)
	}
}
