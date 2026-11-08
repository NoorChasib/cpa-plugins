package wire

import (
	"math"
	"time"
)

// Heartbeat is the body of POST /v1/heartbeat, sent every 60 s and at shutdown by the plugin
// (directly over HTTP, never queued) and at the end of every importer run. Counters are cumulative per
// boot_id.
type Heartbeat struct {
	V           int                  `json:"v"`
	OutboxID    string               `json:"outbox_id"`
	BootID      string               `json:"boot_id"`
	N           uint64               `json:"n"`
	SentAt      time.Time            `json:"sent_at"`
	StartedAt   time.Time            `json:"started_at"`
	Component   string               `json:"component"` // cpa-plugin | importer
	Version     string               `json:"version"`
	State       string               `json:"state"` // running | unconfigured | degraded | stopping (plugin); ok | partial (importer run)
	MaskVersion string               `json:"mask_version"`
	RulesetSHA  string               `json:"ruleset_sha256"`
	ExactRev    uint32               `json:"exact_rev"`
	HostVersion string               `json:"host_version,omitempty"` // CPA schema seen at register, e.g. "schema:10"
	LastHookAt  map[string]time.Time `json:"last_hook_at"`           // before_auth, after_auth, response, stream_header_init, stream_chunk, lifecycle, usage, management
	LastUsageAt *time.Time           `json:"last_usage_at,omitempty"`
	Calls       map[string]uint64    `json:"calls"`
	HookErrors  map[string]uint64    `json:"hook_errors"`
	Panics      map[string]uint64    `json:"recovered_panics"`
	Dropped     map[string]uint64    `json:"dropped"`
	HookP50us   map[string]uint32    `json:"hook_p50_us"`
	HookP99us   map[string]uint32    `json:"hook_p99_us"`
	Outbox      OutboxStats          `json:"outbox"`
	Uploader    UploaderStats        `json:"uploader"`
	Runtime     RuntimeStats         `json:"runtime"`
	Masker      MaskerStats          `json:"masker"`
	Importer    *ImporterStats       `json:"importer,omitempty"`
	Faults      []string             `json:"faults"`
}

// OutboxStats is Heartbeat.Outbox.
type OutboxStats struct {
	Pending            uint64    `json:"pending"`
	PendingBytes       uint64    `json:"pending_bytes"`
	Dead               uint64    `json:"dead"`
	AckedRetained      uint64    `json:"acked_retained"`
	AckedRetainedBytes uint64    `json:"acked_retained_bytes"`
	OldestPendingAgeS  int64     `json:"oldest_pending_age_s"`
	DiskBytes          uint64    `json:"disk_bytes"`
	CapBytes           uint64    `json:"cap_bytes"`
	FreeBytes          uint64    `json:"free_bytes"`
	Pressure           int       `json:"pressure"`        // 0..3
	ReplayWindowH      float64   `json:"replay_window_h"` // actual retained window after any eviction
	NextSeq            uint64    `json:"next_seq"`
	HW                 uint64    `json:"hw"`
	LastAckAt          time.Time `json:"last_ack_at"`
}

// UploaderStats is Heartbeat.Uploader.
type UploaderStats struct {
	LastSuccessAt       time.Time `json:"last_success_at"`
	LastErrorClass      string    `json:"last_error_class"` // net | 5xx | 429 | 401 | 403 | 413 | 409 | none
	ConsecutiveFailures uint32    `json:"consecutive_failures"`
	EpochSeen           uint64    `json:"epoch_seen"`
	ArchiveID           string    `json:"archive_id"`
	ReplayGapFrom       uint64    `json:"replay_gap_from"`
	ReplayGapTo         uint64    `json:"replay_gap_to"`
}

// RuntimeStats is Heartbeat.Runtime.
type RuntimeStats struct {
	HeapInuse         uint64 `json:"heap_inuse"`
	Sys               uint64 `json:"sys"`
	MemLimit          uint64 `json:"mem_limit"`
	InflightBodyBytes uint64 `json:"inflight_body_bytes"`
	StreamAccumBytes  uint64 `json:"stream_accum_bytes"`
	Goroutines        uint64 `json:"goroutines"`
	ActiveStreams     uint64 `json:"active_streams"`
	GCCycles          uint64 `json:"gc_cycles"`
}

// MaskerStats is Heartbeat.Masker.
type MaskerStats struct {
	Bodies        uint64            `json:"bodies"`
	Bytes         uint64            `json:"bytes"`
	MemoHits      uint64            `json:"memo_hits"`
	MemoMisses    uint64            `json:"memo_misses"`
	Timeouts      uint64            `json:"timeouts"`
	P50MS         float64           `json:"p50_ms"`
	P99MS         float64           `json:"p99_ms"`
	MatchesByRule map[string]uint64 `json:"matches_by_rule"` // counts only
}

// ImporterStats is Heartbeat.Importer.
type ImporterStats struct {
	LastScanAt   time.Time `json:"last_scan_at"`
	FilesTracked uint64    `json:"files_tracked"`
	BacklogBytes uint64    `json:"backlog_bytes"`
}

// HeartbeatRow is one archive.heartbeat row minus the three columns the server fills in itself:
// source_id (from the token), ts (server receive time) and body (canon/v1 of the whole heartbeat).
// The ch tags are the column names; the Go types
// match the column types.
type HeartbeatRow struct {
	EpochSeen        uint32            `ch:"epoch_seen"`        // uploader.epoch_seen
	ComponentVersion string            `ch:"component_version"` // component + "/" + version
	HostVersion      string            `ch:"host_version"`      // host_version
	LastHookAt       time.Time         `ch:"last_hook_at"`      // latest value of last_hook_at; Unix 0 when empty
	LastUsageAt      time.Time         `ch:"last_usage_at"`     // last_usage_at; Unix 0 when absent
	OutboxDepth      uint32            `ch:"outbox_depth"`      // outbox.pending
	OutboxBytes      uint64            `ch:"outbox_bytes"`      // outbox.pending_bytes
	OldestPendingS   uint32            `ch:"oldest_pending_s"`  // outbox.oldest_pending_age_s
	Counters         map[string]uint64 `ch:"counters"`          // calls.*, hook_errors.*, recovered_panics.*, dropped.*, outbox.dead, uploader.consecutive_failures
	BootID           string            `ch:"boot_id"`           // boot_id
}

// Row maps h to its archive.heartbeat columns. Values above a UInt32 column's range
// saturate; a negative oldest_pending_age_s becomes 0.
func (h Heartbeat) Row() HeartbeatRow {
	r := HeartbeatRow{
		EpochSeen:        sat32(h.Uploader.EpochSeen),
		ComponentVersion: h.Component + "/" + h.Version,
		HostVersion:      h.HostVersion,
		LastHookAt:       time.Unix(0, 0).UTC(),
		LastUsageAt:      time.Unix(0, 0).UTC(),
		OutboxDepth:      sat32(h.Outbox.Pending),
		OutboxBytes:      h.Outbox.PendingBytes,
		BootID:           h.BootID,
	}
	for _, t := range h.LastHookAt {
		if t.After(r.LastHookAt) {
			r.LastHookAt = t.UTC()
		}
	}
	if h.LastUsageAt != nil {
		r.LastUsageAt = h.LastUsageAt.UTC()
	}
	if h.Outbox.OldestPendingAgeS > 0 {
		r.OldestPendingS = sat32(uint64(h.Outbox.OldestPendingAgeS))
	}
	r.Counters = make(map[string]uint64, len(h.Calls)+len(h.HookErrors)+len(h.Panics)+len(h.Dropped)+2)
	for prefix, m := range map[string]map[string]uint64{
		"calls.": h.Calls, "hook_errors.": h.HookErrors, "recovered_panics.": h.Panics, "dropped.": h.Dropped,
	} {
		for k, v := range m {
			r.Counters[prefix+k] = v
		}
	}
	r.Counters["outbox.dead"] = h.Outbox.Dead
	r.Counters["uploader.consecutive_failures"] = uint64(h.Uploader.ConsecutiveFailures)
	return r
}

func sat32(v uint64) uint32 {
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}
