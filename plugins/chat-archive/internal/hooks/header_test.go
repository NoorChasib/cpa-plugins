package hooks

import (
	"net/http"
	"testing"
)

// TestHeaderList: repeated Content-Encoding field lines combine in order (RFC 9110 5.3), so a body
// gzipped and then br-encoded names both codings; the first line alone would leave it encoded. Keys
// match in any case, for HeaderGet too.
func TestHeaderList(t *testing.T) {
	h := http.Header{"Content-Encoding": {"gzip", "br"}, "Content-Type": {"application/json"}}
	if got := HeaderList(h, "content-encoding"); got != "gzip, br" {
		t.Fatalf("%q", got)
	}
	if got := HeaderList(http.Header{"content-encoding": {"zstd"}}, "Content-Encoding"); got != "zstd" {
		t.Fatalf("lowercase key: %q", got)
	}
	if got := HeaderList(nil, "Content-Encoding"); got != "" {
		t.Fatalf("absent: %q", got)
	}
	if got := HeaderGet(http.Header{"content-type": {"application/json"}}, "Content-Type"); got != "application/json" {
		t.Fatalf("HeaderGet lowercase key: %q", got)
	}
}
