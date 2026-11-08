package canon

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestItemID pins the identity formula sha256(canon({"f":F,"m":M,"p":hex(P)})), absent fields omitted,
// V excluded.
func TestItemID(t *testing.T) {
	p := wire.Hash(sha256.Sum256([]byte("payload")))
	ph := hx(p)
	cases := []struct {
		it   wire.Item
		want string
	}{
		{wire.Item{F: json.RawMessage(`{"type":"text"}`), P: &p}, `{"f":{"type":"text"},"p":"` + ph + `"}`},
		{wire.Item{M: json.RawMessage(`{"role":"user"}`)}, `{"m":{"role":"user"}}`},
		{wire.Item{F: json.RawMessage(`{"type":"image"}`)}, `{"f":{"type":"image"}}`},
		{wire.Item{F: json.RawMessage(`{"type":"text"}`), P: &p, V: json.RawMessage(`{"cache_control":{}}`)}, `{"f":{"type":"text"},"p":"` + ph + `"}`},
		{wire.Item{F: json.RawMessage(` { "type" : "text" } `), P: &p}, `{"f":{"type":"text"},"p":"` + ph + `"}`},
		{wire.Item{M: json.RawMessage(`{"role":"user"}`), F: json.RawMessage(`{"b":1,"a":2}`)}, `{"f":{"a":2,"b":1},"m":{"role":"user"}}`},
		{wire.Item{}, `{}`},
	}
	for _, c := range cases {
		if got, want := ItemID(c.it), sha256.Sum256([]byte(c.want)); got != want {
			t.Errorf("ItemID(%s) is not sha256(%s)", c.it.F, c.want)
		}
	}
	ids := ItemIDs([]wire.Item{cases[0].it, cases[1].it})
	if len(ids) != 2 || ids[0] != ItemID(cases[0].it) || ids[1] != ItemID(cases[1].it) {
		t.Error("ItemIDs")
	}
}

func TestListSHAAndLCP(t *testing.T) {
	a, b, c := wire.Hash{1}, wire.Hash{2}, wire.Hash{3}
	if ListSHA(nil) != sha256.Sum256(nil) {
		t.Error("ListSHA(nil)")
	}
	cat := append(append([]byte{}, a[:]...), b[:]...)
	if ListSHA([]wire.Hash{a, b}) != sha256.Sum256(cat) {
		t.Error("ListSHA is not sha256 of the concatenated ids")
	}
	for _, x := range []struct {
		a, b []wire.Hash
		n    int
	}{
		{nil, nil, 0}, {[]wire.Hash{a}, nil, 0}, {[]wire.Hash{a, b}, []wire.Hash{a, b, c}, 2},
		{[]wire.Hash{a, b, c}, []wire.Hash{a, c, b}, 1}, {[]wire.Hash{b}, []wire.Hash{a}, 0}, {[]wire.Hash{a, b}, []wire.Hash{a, b}, 2},
	} {
		if got := LCP(x.a, x.b); got != x.n {
			t.Errorf("LCP = %d, want %d", got, x.n)
		}
	}
}
