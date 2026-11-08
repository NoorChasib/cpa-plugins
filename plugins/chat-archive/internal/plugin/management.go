package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/hooks"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

const (
	basePath     = "/v0/management/plugins/" + ID
	statusPath   = basePath + "/status"
	requeuePath  = basePath + "/requeue"
	resourcePath = "/v0/resource/plugins/" + ID + "/status"
)

// managementRegistration lists exact routes only; Menu stays empty on data routes (a GET with a
// Menu becomes public). Never …/config, /enabled, /quota or DELETE: CPA's own
// parameterized plugin routes would win over them.
func managementRegistration() protocol.ManagementRegistration {
	return protocol.ManagementRegistration{
		Routes: []protocol.ManagementRoute{
			{Method: http.MethodGet, Path: "/plugins/" + ID + "/status", Description: "Private chat-archive capture and upload health (counts only)."},
			{Method: http.MethodPost, Path: "/plugins/" + ID + "/requeue", Description: "Private: requeue outbox dead letters by reason."},
		},
		Resources: []protocol.ResourceRoute{{Path: "/status", Menu: "Chat Archive", Description: "Chat archive capture health."}},
	}
}

// decodeManagement reads Method, Path and Query only. Headers (which carry the management key)
// and Body are never decoded.
func decodeManagement(raw []byte) (protocol.ManagementRequest, error) {
	var r protocol.ManagementRequest
	err := protocol.Fields(raw, func(k, v []byte) (err error) {
		switch string(k) {
		case "Method":
			r.Method, err = protocol.String(v)
		case "Path":
			r.Path, err = protocol.String(v)
		case "Query":
			if !protocol.IsNull(v) {
				if len(v) > 16<<10 {
					return protocol.ErrSyntax
				}
				var q url.Values
				if json.Unmarshal(v, &q) != nil {
					return protocol.ErrSyntax
				}
				r.Query = q
			}
		}
		return err
	})
	return r, err
}

func (p *Plugin) management(raw []byte) protocol.ManagementResponse {
	start := p.now()
	defer func() { p.engine.Stats.Call(hooks.HookManagement, start, p.now().Sub(start)) }()
	req, err := decodeManagement(raw)
	if err != nil {
		return failure(http.StatusBadRequest, "invalid_request")
	}
	// The public resource returns fixed bytes before looking at any query or state.
	if req.Method == http.MethodGet && req.Path == resourcePath {
		return sidebarResponse()
	}
	switch {
	case req.Method == http.MethodGet && req.Path == statusPath:
		if len(req.Query) > 0 {
			return failure(http.StatusBadRequest, "invalid_query")
		}
		return jsonResponse(http.StatusOK, p.status())
	case req.Method == http.MethodPost && req.Path == requeuePath:
		return p.requeue(req.Query)
	}
	return failure(http.StatusNotFound, "not_found")
}

func failure(status int, code string) protocol.ManagementResponse {
	return jsonResponse(status, map[string]string{"error": code})
}

func jsonResponse(status int, v any) protocol.ManagementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"encoding_failed"}`)
	}
	return protocol.ManagementResponse{StatusCode: status, Body: body, Headers: http.Header{
		"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}}
}

// Requeue reasons accepted by the requeue route: the server rejection reasons and the local
// dead-letter reasons.
var requeueReasons = map[string]bool{
	string(wire.ReasonMaskViolation): true, string(wire.ReasonMaskVersionMissing): true, string(wire.ReasonMaskVersionRetired): true,
	string(wire.ReasonCanonVersion): true, string(wire.ReasonSchema): true, string(wire.ReasonTooLarge): true,
	wire.TombRejectedLocal: true, wire.TombRejectedTooLarge: true, wire.TombMissingRef: true,
}

// requeue validates and enqueues a job for the maintenance goroutine and answers 202 at once
// (409 while a job is queued or running). It never touches SQLite.
func (p *Plugin) requeue(q url.Values) protocol.ManagementResponse {
	flags := map[string]bool{}
	for k, vs := range q {
		if len(vs) != 1 {
			return failure(http.StatusBadRequest, "invalid_query")
		}
		switch k {
		case "reason":
			if !requeueReasons[vs[0]] {
				return failure(http.StatusBadRequest, "invalid_reason")
			}
		case "remask", "full", "split":
			if vs[0] != "0" && vs[0] != "1" {
				return failure(http.StatusBadRequest, "invalid_query")
			}
			flags[k] = vs[0] == "1"
		default:
			return failure(http.StatusBadRequest, "invalid_query")
		}
	}
	reason := q.Get("reason")
	if reason == "" {
		return failure(http.StatusBadRequest, "invalid_reason")
	}
	if flags["full"] || flags["split"] {
		// Re-splitting under new limits and prefix-delta base expansion are not built; an
		// honest refusal beats a silent no-op.
		return failure(http.StatusBadRequest, "unsupported_option")
	}
	if p.currentState() != StateRunning {
		return failure(http.StatusServiceUnavailable, "not_running")
	}
	id, ok := p.jobs.enqueue(reason, flags["remask"], p.now())
	if !ok {
		return failure(http.StatusConflict, "job_running")
	}
	return jsonResponse(http.StatusAccepted, map[string]string{"job": id})
}

// ---- requeue jobs ----

// Job is one requeue job; status shows its progress as counts.
type Job struct {
	ID         string    `json:"id"`
	Reason     string    `json:"reason"`
	Remask     bool      `json:"remask"`
	State      string    `json:"state"` // queued | running | done | failed
	Processed  uint64    `json:"processed"`
	Requeued   uint64    `json:"requeued"`
	Skipped    uint64    `json:"skipped"`
	Failed     uint64    `json:"failed"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	cursor     uint64
}

type jobQueue struct {
	mu   sync.Mutex
	n    uint64
	cur  *Job
	done []Job
}

func (q *jobQueue) enqueue(reason string, remask bool, now time.Time) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cur != nil {
		return "", false
	}
	q.n++
	q.cur = &Job{ID: "rq-" + strconv.FormatUint(q.n, 10), Reason: reason, Remask: remask, State: "queued", CreatedAt: now.UTC()}
	return q.cur.ID, true
}

func (q *jobQueue) snapshot() []Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := append([]Job{}, q.done...)
	if q.cur != nil {
		out = append(out, *q.cur)
	}
	return out
}
