package wire

// Status is the per-seq result of a commit; exactly one of the constants below.
type Status string

// Commit statuses. `quarantined` is a derive state, not an ingest status.
const (
	StatusOK        Status = "ok"
	StatusDuplicate Status = "duplicate"
	StatusConflict  Status = "conflict"
	StatusRejected  Status = "rejected"
	StatusRetry     Status = "retry"
)

// Reason qualifies a rejected or retry status.
type Reason string

// Reasons for StatusRejected.
const (
	ReasonMaskViolation      Reason = "mask_violation"
	ReasonMaskVersionMissing Reason = "mask_version_missing"
	ReasonMaskVersionRetired Reason = "mask_version_retired"
	ReasonCanonVersion       Reason = "canon_version"
	ReasonSchema             Reason = "schema"
	ReasonTooLarge           Reason = "too_large"
)

// Reasons for StatusRetry.
const (
	ReasonMissingRef       Reason = "missing_ref"
	ReasonBusy             Reason = "busy"
	ReasonStoreUnavailable Reason = "store_unavailable"
)

// CommitResponse is the 200 body of POST /v1/ingest/commit.
type CommitResponse struct {
	Epoch     uint64            `json:"epoch"`
	ArchiveID string            `json:"archive_id"`
	HW        map[string]uint64 `json:"hw"`      // outbox_id -> hw
	Staged    []Hash            `json:"staged"`  // every acked piece hash that sits only in the staging tier
	Results   []CommitResult    `json:"results"` // exactly one per seq in the batch, tombstones included
}

// CommitResult is one entry of CommitResponse.Results.
type CommitResult struct {
	Seq     uint64 `json:"seq"`
	Status  Status `json:"status"`
	Reason  Reason `json:"reason,omitempty"`
	Missing []Hash `json:"missing,omitempty"` // unresolved refs of a retry/missing_ref
}

// ErrorBody is the JSON body of a non-2xx answer: {"error":"epoch_changed","epoch":N,"archive_id":"…"}
// for 409, {"reason":"mask_violation","rules":[…]} for a 422 piece, {"error":"<code>"} otherwise.
// One struct for every error body the archive sends.
type ErrorBody struct {
	Error     string   `json:"error,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Rules     []string `json:"rules,omitempty"`
	Epoch     uint64   `json:"epoch,omitempty"`
	ArchiveID string   `json:"archive_id,omitempty"`
}

// ErrorBody.Error codes the client maps to typed errors.
const (
	ErrCodeEpochChanged        = "epoch_changed"
	ErrCodePieceHashMismatch   = "piece_hash_mismatch"
	ErrCodeUnsupportedProtocol = "unsupported_protocol"
)
