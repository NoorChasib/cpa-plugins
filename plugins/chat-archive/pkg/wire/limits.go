package wire

import "time"

// Limits are the transport limits the server returns in GET /v1/sources/self. Byte
// limits are raw (uncompressed) bytes.
type Limits struct {
	EnvelopeJSON    int64 `json:"envelope_json"`
	PieceInline     int64 `json:"piece_inline"`
	TextPiece       int64 `json:"text_piece"`
	Attachment      int64 `json:"attachment"`
	CommitRaw       int64 `json:"commit_raw"`
	CommitEnvelopes int   `json:"commit_envelopes"`
	NeedHashes      int   `json:"need_hashes"`
	Heartbeat       int64 `json:"heartbeat"`
}

// DefaultLimits are the default limits; a server returns the ones it applies in GET /v1/sources/self.
var DefaultLimits = Limits{
	EnvelopeJSON:    1 << 20,
	PieceInline:     4 << 20,
	TextPiece:       64 << 20,
	Attachment:      50_000_000,
	CommitRaw:       32 << 20,
	CommitEnvelopes: 2000,
	NeedHashes:      32768,
	Heartbeat:       64 << 10,
}

// Producer batching targets: a producer aims for commits of 8 MiB or 500 envelopes, lingering at most
// 1 s.
const (
	TargetCommitBytes     = 8 << 20
	TargetCommitEnvelopes = 500
	TargetCommitLinger    = time.Second
)
