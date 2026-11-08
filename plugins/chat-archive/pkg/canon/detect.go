package canon

import (
	"fmt"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// DetectPiece is the server's detect-only re-check of one piece, run the
// way the producer masked it: a text piece as one string; a json piece on every string value and
// every object key, each decoded; an attachment as one string when its bytes are valid UTF-8, each
// maximal valid UTF-8 run when contentType is a declared text type (text/*, *+json, *+xml,
// application/json, application/x-yaml, application/xml), and not at all otherwise (binary: a known gap,
// a secret inside binary bytes stays unmasked). Running Detect over the serialized bytes of a json piece instead would flag values the
// producer masked correctly, because a key and its value are masked as separate strings. The same
// call with kind wire.PieceJSON checks an envelope body. err is non-nil for a json piece that does
// not parse and for an unknown kind.
func DetectPiece(kind wire.PieceKind, contentType string, b []byte, m *mask.Masker) ([]mask.Finding, error) {
	if m == nil {
		return nil, errNoMasker
	}
	switch kind {
	case wire.PieceText:
		return m.Detect(string(b)), nil
	case wire.PieceJSON:
		n, _, err := parse(b)
		if err != nil {
			return nil, fmt.Errorf("canon: DetectPiece: %w", err)
		}
		var out []mask.Finding
		detectStrings(n, m, &out)
		return out, nil
	case wire.PieceAttachment:
		if utf8.Valid(b) {
			return m.Detect(string(b)), nil
		}
		if !isTextMedia(normMediaType(contentType)) {
			return nil, nil
		}
		var out []mask.Finding
		utf8Runs(b, func(seg []byte, valid bool) {
			if valid {
				out = append(out, m.Detect(string(seg))...)
			}
		})
		return out, nil
	}
	return nil, fmt.Errorf("canon: DetectPiece: unknown piece kind %d", kind)
}

func detectStrings(n *node, m *mask.Masker, out *[]mask.Finding) {
	switch n.kind {
	case kString:
		*out = append(*out, m.Detect(n.s)...)
	case kObject:
		for _, mb := range n.members {
			*out = append(*out, m.Detect(mb.key)...)
			detectStrings(mb.val, m, out)
		}
	case kArray:
		for _, el := range n.elems {
			detectStrings(el, m, out)
		}
	}
}
