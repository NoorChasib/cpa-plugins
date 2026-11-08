package wire

import (
	"net/http"
	"strconv"
	"time"
)

// Route paths producers call.
const (
	PathHealthz         = "/healthz"
	PathSourcesSelf     = "/v1/sources/self"
	PathNeed            = "/v1/ingest/need"
	PathPiecePrefix     = "/v1/pieces/" // + 64 lowercase hex
	PathCommit          = "/v1/ingest/commit"
	PathHeartbeat       = "/v1/heartbeat"
	PathMediaFetch      = "/v1/media-fetch"
	PathSessionCoverage = "/v1/ingest/session-coverage"
	PathEnroll          = "/v1/enroll"
	PathCallerPrefix    = "/v1/callers/" // + caller_fp
)

// HTTP header names.
const (
	HeaderEpoch      = "X-Archive-Epoch"       // every source-authenticated response
	HeaderArchiveID  = "X-Archive-Archive-Id"  // every source-authenticated response; also sent on commit
	HeaderServerTime = "X-Archive-Server-Time" // every source-authenticated response, RFC 3339 UTC with nanoseconds
	HeaderEpochSeen  = "X-Archive-Epoch-Seen"  // sent on commit
	HeaderPieceKind  = "X-Archive-Piece-Kind"  // sent on PUT /v1/pieces/{sha256}: json | text | attachment
)

// ServerState is parsed from the X-Archive-Epoch, X-Archive-Archive-Id and X-Archive-Server-Time
// headers of every response.
type ServerState struct {
	Epoch      uint64    `json:"epoch"`
	ArchiveID  string    `json:"archive_id"`
	ServerTime time.Time `json:"server_time"`
}

// ParseServerState reads ServerState from response headers. ok is false when the epoch or archive id
// header is missing or the epoch does not parse (for example /healthz or a 401).
//
// A missing or unparsable X-Archive-Server-Time leaves ServerTime zero and does not clear ok.
func ParseServerState(h http.Header) (st ServerState, ok bool) {
	epoch, id := h.Get(HeaderEpoch), h.Get(HeaderArchiveID)
	if epoch == "" || id == "" {
		return ServerState{}, false
	}
	n, err := strconv.ParseUint(epoch, 10, 64)
	if err != nil {
		return ServerState{}, false
	}
	st = ServerState{Epoch: n, ArchiveID: id}
	if t, err := time.Parse(time.RFC3339Nano, h.Get(HeaderServerTime)); err == nil {
		st.ServerTime = t.UTC()
	}
	return st, true
}

// NeedRequest is the body of POST /v1/ingest/need: at most limits.need_hashes hashes.
type NeedRequest struct {
	Pieces []Hash `json:"pieces"`
}

// NeedResponse is the 200 body of POST /v1/ingest/need.
type NeedResponse struct {
	Need   []Hash `json:"need"`
	Staged []Hash `json:"staged"` // present only in the staging tier, staged_at <= 3 days ago; never have-cached
}

// SourcesSelf is the 200 body of GET /v1/sources/self.
type SourcesSelf struct {
	SourceID   string            `json:"source_id"`
	Kind       string            `json:"kind"` // cpa-plugin | importer
	Label      string            `json:"label"`
	ArchiveID  string            `json:"archive_id"`
	Epoch      uint64            `json:"epoch"`
	Outboxes   map[string]uint64 `json:"outboxes"` // outbox_id -> hw
	Accepted   Accepted          `json:"accepted"`
	Exact      ExactSet          `json:"exact"`
	Limits     Limits            `json:"limits"`
	ServerTime time.Time         `json:"server_time"`
}

// Accepted lists the versions the server accepts.
type Accepted struct {
	Protocol []int                     `json:"protocol"` // [1]
	Canon    []string                  `json:"canon"`    // ["canon/v1"]
	Mask     map[string]MaskAcceptance `json:"mask"`     // {"mask/v1":{"created_before":null}}
}

// MaskAcceptance is one accepted mask version. An envelope is accepted when CreatedBefore is nil or its
// CreatedAt is earlier.
type MaskAcceptance struct {
	CreatedBefore *time.Time `json:"created_before"`
}

// ExactSet is the server's exact-value set. FPs are ExactFP values and CallerFPs
// raw CallerScope values, both 64 hex in JSON; they feed mask.Config.
type ExactSet struct {
	Rev       uint32 `json:"rev"`
	FPs       []Hash `json:"fps"`
	CallerFPs []Hash `json:"caller_fps"`
}

// EnrollRequest is the body of POST /v1/enroll.
type EnrollRequest struct {
	Code          string `json:"code"`
	Kind          string `json:"kind"` // importer
	MachineID     string `json:"machine_id"`
	Hostname      string `json:"hostname"`
	Label         string `json:"label"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	ClientVersion string `json:"client_version"`
	Intermittent  bool   `json:"intermittent"`
	CallerFP      string `json:"caller_fp,omitempty"`
}

// EnrollResponse is the 200 body of POST /v1/enroll.
type EnrollResponse struct {
	SourceID  string `json:"source_id"`
	Token     string `json:"token"`
	ArchiveID string `json:"archive_id"`
	Epoch     uint64 `json:"epoch"`
}

// CallerRequest is the body of PUT /v1/callers/{caller_fp}; Machine matches ^[A-Za-z0-9._@-]{1,64}$.
type CallerRequest struct {
	Machine   string     `json:"machine"`
	App       string     `json:"app,omitempty"`
	ValidFrom *time.Time `json:"valid_from,omitempty"`
	Note      string     `json:"note,omitempty"`
	Force     bool       `json:"force,omitempty"`
}

// CallerResponse is the 200 body of PUT /v1/callers/{caller_fp}.
type CallerResponse struct {
	Machine string `json:"machine"`
}

// CoverageRequest is the body of POST /v1/ingest/session-coverage: at most MaxCoverageIDs ids such as
// "codex:<id>".
type CoverageRequest struct {
	SessionIDs []string `json:"session_ids"`
}

// CoverageResponse is the 200 body of POST /v1/ingest/session-coverage.
type CoverageResponse struct {
	Sessions map[string]Coverage `json:"sessions"`
}

// Coverage is what CPA captured for one session id. Responses lists the Codex
// response ids CPA captured with content.
type Coverage struct {
	CPASeen         bool       `json:"cpa_seen"`
	FirstCapturedAt *time.Time `json:"first_captured_at"`
	Exchanges       uint64     `json:"exchanges"`
	Responses       []string   `json:"responses"`
}

// HeartbeatResponse is the 200 body of POST /v1/heartbeat.
type HeartbeatResponse struct {
	Epoch      uint64    `json:"epoch"`
	ArchiveID  string    `json:"archive_id"`
	ExactRev   uint32    `json:"exact_rev"`
	ServerTime time.Time `json:"server_time"`
}

// Fixed route caps.
const (
	MaxCoverageIDs    = 1000     // session ids per session-coverage call
	MaxMediaFetchJSON = 16 << 10 // bytes of one media-fetch body
)
