// Package wire is protocol v1 of the chat archive: envelope types, idempotency keys, the CAB1 commit
// codec, limits, statuses and the bodies of the HTTP routes producers call. It is
// pure Go, imports nothing from this module, and is imported by the plugin, the importer and the
// private server.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package wire

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// Protocol and content versions.
const (
	ProtocolV1 = 1
	CanonV1    = "canon/v1"
	MaskV1     = "mask/v1"
)

// Hash is a sha256 digest. JSON carries it as 64 lowercase hex characters; CAB1 carries the 32 raw
// bytes.
type Hash [32]byte

// String returns h as 64 lowercase hex characters.
func (h Hash) String() string {
	return hex.EncodeToString(h[:])
}

// MarshalText encodes h as 64 lowercase hex characters, so every Hash, *Hash and []Hash field
// marshals as hex in JSON.
func (h Hash) MarshalText() ([]byte, error) {
	out := make([]byte, hex.EncodedLen(len(h)))
	hex.Encode(out, h[:])
	return out, nil
}

// UnmarshalText decodes exactly 64 lowercase hex characters; anything else is an error.
func (h *Hash) UnmarshalText(text []byte) error {
	if len(text) != 2*len(h) {
		return fmt.Errorf("wire: hash must be 64 lowercase hex characters, got %d bytes", len(text))
	}
	for _, c := range text {
		if !isLowerHex(c) {
			return errors.New("wire: hash must be 64 lowercase hex characters")
		}
	}
	var out Hash
	if _, err := hex.Decode(out[:], text); err != nil {
		return fmt.Errorf("wire: hash: %w", err)
	}
	*h = out
	return nil
}

func isLowerHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f')
}

// ParseHash decodes exactly 64 lowercase hex characters, for example the {sha256} path segment of
// PUT /v1/pieces/{sha256}.
func ParseHash(s string) (Hash, error) {
	var h Hash
	err := h.UnmarshalText([]byte(s))
	return h, err
}

// PieceKind is the wire kind of a piece. The values are the CAB1 kind byte.
type PieceKind uint8

// Piece kinds.
const (
	PieceJSON       PieceKind = 0
	PieceText       PieceKind = 1
	PieceAttachment PieceKind = 2
)

// String returns "json", "text" or "attachment": the X-Archive-Piece-Kind header value of
// PUT /v1/pieces.
func (k PieceKind) String() string {
	switch k {
	case PieceJSON:
		return "json"
	case PieceText:
		return "text"
	case PieceAttachment:
		return "attachment"
	}
	return "PieceKind(" + strconv.Itoa(int(k)) + ")"
}

// valid reports whether k is one of the three wire kinds.
func (k PieceKind) valid() bool {
	return k <= PieceAttachment
}

// ParsePieceKind parses an X-Archive-Piece-Kind header value: json, text or attachment.
func ParsePieceKind(s string) (PieceKind, error) {
	switch s {
	case "json":
		return PieceJSON, nil
	case "text":
		return PieceText, nil
	case "attachment":
		return PieceAttachment, nil
	}
	return 0, fmt.Errorf("wire: unknown piece kind %q", s)
}

// Kind is an envelope kind.
type Kind string

// Envelope kinds.
const (
	KindExchangeBegin    Kind = "exchange.begin"
	KindExchangeAttempt  Kind = "exchange.attempt"
	KindExchangeResponse Kind = "exchange.response"
	KindExchangeDone     Kind = "exchange.completion"
	KindUsage            Kind = "usage.attempt"
	KindImportRecord     Kind = "import.record"
	KindImportFile       Kind = "import.file"
)

// SourceScoped reports whether keys of kind k are namespaced by source_id (key_ns = source_id):
// true for exchange.* and usage.attempt, false for import.* (key_ns is the empty string).
func (k Kind) SourceScoped() bool {
	switch k {
	case KindExchangeBegin, KindExchangeAttempt, KindExchangeResponse, KindExchangeDone, KindUsage:
		return true
	}
	return false
}

// known reports whether k is one of the seven v1 kinds.
func (k Kind) known() bool {
	return k.SourceScoped() || k == KindImportRecord || k == KindImportFile
}
