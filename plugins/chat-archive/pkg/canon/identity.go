package canon

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// ItemID returns sha256(canon({"f":F,"m":M,"p":hex(P)})) with absent fields omitted; V is never part
// of the identity.
func ItemID(it wire.Item) wire.Hash {
	b := []byte{'{'}
	sep := func() {
		if len(b) > 1 {
			b = append(b, ',')
		}
	}
	if len(it.F) > 0 {
		b = append(b, `"f":`...)
		b = append(b, it.F...)
	}
	if len(it.M) > 0 {
		sep()
		b = append(b, `"m":`...)
		b = append(b, it.M...)
	}
	if it.P != nil {
		sep()
		b = append(b, `"p":"`...)
		b = hex.AppendEncode(b, it.P[:])
		b = append(b, '"')
	}
	b = append(b, '}')
	// F and M from this package are canon/v1 already, so b is too; Canonicalize covers frames that
	// were built elsewhere. A frame that is not JSON at all is hashed as given.
	if out, _, err := Canonicalize(b); err == nil {
		b = out
	}
	return sha256.Sum256(b)
}

// ItemIDs returns ItemID of every item, in order.
func ItemIDs(items []wire.Item) []wire.Hash {
	ids := make([]wire.Hash, len(items))
	for i, it := range items {
		ids[i] = ItemID(it)
	}
	return ids
}

// ListSHA returns sha256 of the concatenated 32-byte ids.
func ListSHA(ids []wire.Hash) wire.Hash {
	h := sha256.New()
	for i := range ids {
		h.Write(ids[i][:])
	}
	var out wire.Hash
	h.Sum(out[:0])
	return out
}

// LCP returns the length of the longest common prefix of a and b.
func LCP(a, b []wire.Hash) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
