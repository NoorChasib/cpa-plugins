package wire

import (
	"crypto/sha256"
	"encoding/json"
	"time"
)

// Envelope is the unit of idempotency, ordering and acknowledgement.
//
// body_sha256 (server receipts, duplicate/conflict decisions) = sha256 of the Body bytes exactly as
// sent. Nothing outside Body (v, seq, epoch_seen, exact_rev, refs, loc) is part of it, so replays and
// re-reads of the same content at another position are `duplicate`, never `conflict`.
type Envelope struct {
	V            int             `json:"v"` // 1
	Kind         Kind            `json:"kind"`
	OutboxID     string          `json:"outbox_id"`
	Seq          uint64          `json:"seq"` // >= 1
	EpochSeen    uint64          `json:"epoch_seen"`
	Key          string          `json:"key"`
	MaskVersion  string          `json:"mask_version"`  // required
	CanonVersion string          `json:"canon_version"` // required
	ExactRev     uint32          `json:"exact_rev"`
	CreatedAt    time.Time       `json:"created_at"`     // immutable per key: hook time, or the record's own timestamp
	Refs         []Hash          `json:"refs,omitempty"` // every piece and attachment the body references
	Loc          *Locator        `json:"loc,omitempty"`  // import kinds only (required there); never part of Body
	Body         json.RawMessage `json:"body"`           // canon/v1 of the kind's struct
}

// BodySHA256 is body_sha256: sha256 of e.Body exactly as sent. Nothing outside Body (v, seq,
// epoch_seen, exact_rev, refs, loc) enters it.
func (e Envelope) BodySHA256() Hash {
	return sha256.Sum256(e.Body)
}

// Locator says where an import record or file was read. Per machine and position; one key may have
// many.
type Locator struct {
	MachineID   string     `json:"machine_id"`
	FileKey     string     `json:"file_key"`               // "<root>/<relative path>", root in {claude-projects, codex-sessions, claude-skills}
	ScanGen     uint32     `json:"scan_gen"`               // the importer's per-file rescan counter (0 = first scan)
	LineOrdinal int64      `json:"line_ordinal,omitempty"` // records
	ByteOffset  int64      `json:"byte_offset,omitempty"`  // records
	ByteLen     int64      `json:"byte_len,omitempty"`     // records
	EndOffset   int64      `json:"end_offset,omitempty"`   // records
	Size        int64      `json:"size,omitempty"`         // files
	ModTime     *time.Time `json:"mod_time,omitempty"`     // files
}

// Tombstone is CAB1 record type 3. A producer sends one for every seq it dead-lettered locally (it
// never reached the server as an envelope), so hw can pass that seq.
type Tombstone struct {
	V        int    `json:"v"` // 1
	OutboxID string `json:"outbox_id"`
	Seq      uint64 `json:"seq"`
	Key      string `json:"key"`
	Reason   string `json:"reason"` // rejected_local | rejected_too_large | missing_ref
}

// Tombstone.Reason values.
const (
	TombRejectedLocal    = "rejected_local"
	TombRejectedTooLarge = "rejected_too_large"
	TombMissingRef       = "missing_ref"
)

// Item is one element of an ordered content sequence: a message frame (M set) or a block (F set,
// P optional). Per message: one M item, then one item per content block.
//
// Item identity is canon.ItemID; V is never part of it.
type Item struct {
	// M is the message frame: the message object without its content (role, name, tool_call_id,
	// content_form, phase, ...); canon/v1, masked.
	M json.RawMessage `json:"m,omitempty"`
	// F is the block frame: the block object minus its payload field; canon/v1, masked, with $att and
	// $opaque stubs applied.
	F json.RawMessage `json:"f,omitempty"`
	// P is the payload piece (json or text).
	P *Hash `json:"p,omitempty"`
	// V holds volatile fields removed before hashing (cache_control, prompt_cache_breakpoint);
	// canon/v1, masked like every other string.
	V json.RawMessage `json:"v,omitempty"`
}

// Base is a prefix-delta reference.
type Base struct {
	Key  string `json:"key"`  // key of an earlier exchange.begin from the same (source, outbox)
	Resp int32  `json:"resp"` // response index whose items extend S(base); -1 = input items only
	Kept uint32 `json:"kept"` // items reused from S(base)
}

// Body is the structural split of one request or response body.
type Body struct {
	Format   string          `json:"format"`
	RawLen   int64           `json:"raw_len"`
	Skeleton *Hash           `json:"skeleton,omitempty"`
	Tools    *Hash           `json:"tools,omitempty"`
	ToolsV   json.RawMessage `json:"tools_v,omitempty"`
	System   []Item          `json:"system,omitempty"`
	Base     *Base           `json:"base,omitempty"`
	Items    []Item          `json:"items,omitempty"`   // new items after the kept prefix (all items when Base is nil)
	N        uint32          `json:"n"`                 // total item count after expansion
	ListSHA  Hash            `json:"list_sha"`          // ListSHA of all N item ids
	Flags    []string        `json:"flags,omitempty"`   // dup_keys, bad_utf8, args_raw, gunzipped, legacy_completions, ...
	Dropped  string          `json:"dropped,omitempty"` // body_budget | mask_timeout | oversize | parse_error | disk_guard: metadata only
}

// Body.Flags values.
const (
	FlagDupKeys           = "dup_keys"
	FlagBadUTF8           = "bad_utf8"
	FlagArgsRaw           = "args_raw"
	FlagGunzipped         = "gunzipped"
	FlagLegacyCompletions = "legacy_completions"
	FlagInputParseError   = "input_parse_error"
)

// Body.Dropped values.
const (
	DroppedBodyBudget  = "body_budget"
	DroppedMaskTimeout = "mask_timeout"
	DroppedOversize    = "oversize"
	DroppedParseError  = "parse_error"
	DroppedDiskGuard   = "disk_guard"
)

// Header is one kept request or response header.
type Header struct {
	Name   string   `json:"name"`             // lowercase
	Values []string `json:"values,omitempty"` // masked
	SHA256 *Hash    `json:"sha256,omitempty"`
	Len    int      `json:"len,omitempty"`
	FP     string   `json:"fp,omitempty"` // "k1:<hex>"
}

// ArchiveHints are the X-Archive-* request header values, masked, each at most 256 printable ASCII
// characters; the headers are then cleared upstream.
type ArchiveHints struct {
	Machine    string `json:"machine,omitempty"`
	Origin     string `json:"origin,omitempty"`
	CIRun      string `json:"ci_run,omitempty"`
	CIJob      string `json:"ci_job,omitempty"`
	CIWorkflow string `json:"ci_workflow,omitempty"`
	Runner     string `json:"runner,omitempty"`
	Canary     string `json:"canary,omitempty"`
	Unknown    int    `json:"unknown,omitempty"`
}
