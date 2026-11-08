// Package canon is canon/v1: the one canonical JSON encoder, the per-format structural split into
// items and pieces, opaque and attachment stubs, item identity and prefix-delta helpers, piece roles
// and Claude Code detection. It is pure Go, built on encoding/json/jsontext; it imports
// pkg/wire, pkg/mask and pkg/headers only. The plugin, the importer and the server use only this
// package to canonicalize, so they hash byte-identically.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package canon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var errNoMasker = errors.New("canon: a Masker is required")

// Errors a split returns instead of a Split, so the producer stores metadata only.
var (
	// ErrUndecodableBody: the body is still content-encoded, so no piece may be made of it (its
	// plaintext would sit unmasked inside a stored blob). That is a gzip body that does not decode
	// (bad header, truncated or corrupt stream) or a Content-Encoding other than empty, identity or
	// gzip. The producer records Dropped parse_error.
	ErrUndecodableBody = errors.New("canon: body is still content-encoded")
	// ErrBodyBudget: the body is over the split's budget, Options.MaxDecoded decoded bytes of a gzip
	// body or Options.MaxNodes parsed JSON values. The producer records Dropped body_budget.
	ErrBodyBudget = errors.New("canon: body over the split budget")
)

// Version is the canonicalization this package implements.
const Version = wire.CanonV1

// Body.Format values. A media format is FormatMediaPrefix + "<route>".
const (
	FormatClaude          = "claude.messages"
	FormatOpenAIChat      = "openai.chat"
	FormatOpenAIResponses = "openai.responses"
	FormatGemini          = "gemini.generate"
	FormatInteractions    = "interactions"
	FormatMediaPrefix     = "media."
	FormatMultipart       = "multipart"
	FormatOpaque          = "opaque"
)

// Size thresholds of the split.
const (
	// MinAttachment: inline base64 and data URLs decoding to at least this many bytes become
	// attachments; smaller payloads stay inline in the frame.
	MinAttachment = 1 << 10
	// MaxGunzip caps the decompressed size of a gzip body (non-JSON bodies); Options.MaxDecoded can
	// lower it.
	MaxGunzip = 256 << 20
)

// Marshal returns the canon/v1 encoding of v: encoding/json's output re-encoded by Canonicalize.
// Used for envelope bodies, envelopes, tombstones and heartbeats built from Go values.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, _, err := Canonicalize(b)
	return out, err
}

// Canonicalize re-encodes one JSON value as canon/v1 without masking: no insignificant whitespace,
// array order kept, object keys sorted by UTF-8 bytes (duplicates: last wins, flag dup_keys), minimal
// string escaping, invalid UTF-8 and lone surrogates as U+FFFD (flag bad_utf8), number literals
// verbatim. flags holds wire.FlagDupKeys and/or wire.FlagBadUTF8.
func Canonicalize(raw []byte) (out []byte, flags []string, err error) {
	n, p, err := parse(raw)
	if err != nil {
		return nil, nil, err
	}
	var e encoder
	e.value(n)
	return e.buf, parseFlags(p, false), nil
}

// parseFlags lists the canon/v1 flags in sorted order.
func parseFlags(p parser, maskedDup bool) []string {
	var flags []string
	if p.bad {
		flags = append(flags, wire.FlagBadUTF8)
	}
	if p.dup || maskedDup {
		flags = append(flags, wire.FlagDupKeys)
	}
	return flags
}

// CanonicalizeMasked is Canonicalize with m.String applied to every string value and every object
// key. It does no structural split, attachment decoding or opaque stubbing; the
// importer uses it for record frames and kept structured fields.
func CanonicalizeMasked(raw []byte, m *mask.Masker) (out []byte, hits mask.Hits, flags []string, err error) {
	if m == nil {
		return nil, nil, nil, errNoMasker
	}
	return canonicalizeMasked(raw, m)
}

func canonicalizeMasked(raw []byte, m stringMasker) ([]byte, mask.Hits, []string, error) {
	n, p, err := parse(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	var hits mask.Hits
	e := encoder{mask: func(s string) string {
		out, h := m.String(s)
		hits = addHits(hits, h)
		return out
	}}
	e.value(n)
	return e.buf, hits, parseFlags(p, e.dup), nil
}

// CanonicalizeMaskedContext is CanonicalizeMasked that a context can stop and a node budget can
// bound, as the split is: every string value and object key is masked through m.StringContext, so
// ctx ending stops one long string too, and parsing more than maxNodes JSON values (every object,
// array, string, number and literal; maxNodes <= 0 means no cap) fails with ErrBodyBudget before
// anything is masked. When ctx ends it returns ctx.Err() and nothing else, so no partly masked output
// comes back.
func CanonicalizeMaskedContext(ctx context.Context, raw []byte, m *mask.Masker, maxNodes int) (out []byte, hits mask.Hits, flags []string, err error) {
	if m == nil {
		return nil, nil, nil, errNoMasker
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if maxNodes <= 0 {
		maxNodes = -1
	}
	n, p, err := parseBudget(raw, maxNodes)
	if err != nil {
		return nil, nil, nil, err
	}
	var maskErr error
	e := encoder{mask: func(s string) string {
		if maskErr != nil {
			return ""
		}
		out, h, err := m.StringContext(ctx, s)
		if err != nil {
			maskErr = err
			return ""
		}
		hits = addHits(hits, h)
		return out
	}}
	e.value(n)
	if maskErr != nil {
		return nil, nil, nil, maskErr
	}
	return e.buf, hits, parseFlags(p, e.dup), nil
}

// stringMasker is the part of *mask.Masker the split uses.
type stringMasker interface {
	String(s string) (string, mask.Hits)
}

// ctxMasker is implemented by *mask.Masker: StringContext lets the split's context stop one long
// masking call (the plugin's masking deadline).
type ctxMasker interface {
	StringContext(ctx context.Context, s string) (string, mask.Hits, error)
}

func addHits(dst, src mask.Hits) mask.Hits {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(mask.Hits, len(src))
	}
	for k, v := range src {
		dst[k] += v
	}
	return dst
}

// Piece is one content-addressed piece: Hash = sha256(Bytes).
type Piece struct {
	Hash        wire.Hash
	Kind        wire.PieceKind
	ContentType string // attachments only: the mime type; else empty
	Bytes       []byte // json: canon/v1 bytes; text: masked UTF-8; attachment: decoded (text media: masked) bytes
}

// NewPiece returns the Piece for b with Hash = sha256(b). The caller supplies already-masked,
// already-canonical bytes.
func NewPiece(kind wire.PieceKind, contentType string, b []byte) Piece {
	return Piece{Hash: sha256.Sum256(b), Kind: kind, ContentType: contentType, Bytes: b}
}

// Stub object keys: a frame holds {"$att":{…}} in place of an attachment payload and
// {"$opaque":{…}} in place of an opaque field.
const (
	AttKey    = "$att"
	OpaqueKey = "$opaque"
)

// AttStub is the value under "$att": a decoded attachment, or one over limits.attachment with
// Dropped set and no piece.
type AttStub struct {
	SHA256  wire.Hash `json:"sha256"`            // sha256 of the attachment piece (masked bytes for text media)
	MIME    string    `json:"mime"`              // media type
	Len     int64     `json:"len"`               // byte length of the attachment piece
	Enc     string    `json:"enc"`               // base64 | dataurl | raw (a non-JSON body or multipart file part)
	Masked  bool      `json:"masked,omitempty"`  // true for text media types masked before hashing
	Dropped string    `json:"dropped,omitempty"` // "oversize": over limits.attachment, no piece sent
}

// OpaqueStub is the value under "$opaque": the sha256 and byte length of the exact UTF-8 string as
// sent (base64 text, not decoded).
type OpaqueStub struct {
	SHA256 wire.Hash `json:"sha256"`
	Len    int64     `json:"len"`
}
