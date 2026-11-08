// Command fakearchive runs the clienttest fake archive on loopback for the native acceptance harness
// (spec section 9.3), with a small control API the Python harness uses to inspect what the plugin
// uploaded. It is test-only: it never reaches a real archive and only ever listens on 127.0.0.1.
//
//	fakearchive serve -state FILE   start the fake archive and the control API; FILE receives
//	                                {"url","control","source_id","token"} (0600) once both listen
//	fakearchive unzstd              decode one zstd stream from stdin to stdout
//	fakearchive maskcheck           read {"values":[...],"exact":[...],"callers":[...]} on stdin and
//	                                print, per value, whether mask/v1 changes it and the rule ids
//	                                that fired (never the value), so the harness only plants
//	                                canaries the masker is meant to catch
//
// Control API (all JSON): GET /envelopes, /tombstones, /heartbeats, /media, /hw?outbox=,
// /piece?h=<hex>, /stats; POST /scan {"needles":{name:b64}}, /unzstd (raw body), /fail_next,
// /hold, /release, /exact. /scan answers with counts only, so no content leaves this process
// through the harness output.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: fakearchive serve -state FILE | unzstd")
	}
	switch os.Args[1] {
	case "serve":
		if len(os.Args) != 4 || os.Args[2] != "-state" {
			fail("usage: fakearchive serve -state FILE")
		}
		serve(os.Args[3])
	case "unzstd":
		out, err := unzstd(os.Stdin)
		if err != nil {
			fail("unzstd: " + err.Error())
		}
		_, _ = os.Stdout.Write(out)
	case "maskcheck":
		maskcheck()
	default:
		fail("unknown command")
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

func unzstd(r io.Reader) ([]byte, error) {
	dec, err := zstd.NewReader(r, zstd.WithDecoderMaxMemory(1<<30), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return io.ReadAll(dec)
}

type control struct {
	fake     *clienttest.Server
	sourceID string

	mu    sync.Mutex
	holds map[string]func()
	n     int
}

func serve(stateFile string) {
	fake := clienttest.NewServer(clienttest.Options{})
	sourceID, token := fake.AddSource("cpa-plugin", "native-acceptance")
	c := &control{fake: fake, sourceID: sourceID, holds: map[string]func(){}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("control listen: " + err.Error())
	}
	state, _ := json.Marshal(map[string]string{"url": fake.URL, "control": "http://" + ln.Addr().String(),
		"source_id": sourceID, "token": token})
	tmp := stateFile + ".tmp"
	if err := os.WriteFile(tmp, state, 0o600); err != nil {
		fail("state: " + err.Error())
	}
	if err := os.Rename(tmp, stateFile); err != nil {
		fail("state: " + err.Error())
	}
	srv := &http.Server{Handler: http.HandlerFunc(c.handle), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail("control: " + err.Error())
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type envelopeView struct {
	Status   wire.Status     `json:"status"`
	Reason   wire.Reason     `json:"reason,omitempty"`
	RawLen   int             `json:"raw_len"`
	Envelope json.RawMessage `json:"envelope"`
}

func (c *control) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /envelopes":
		var out []envelopeView
		for _, rc := range c.fake.Envelopes(c.sourceID) {
			raw, _ := json.Marshal(rc.Envelope)
			out = append(out, envelopeView{Status: rc.Status, Reason: rc.Reason, RawLen: len(rc.Raw), Envelope: raw})
		}
		writeJSON(w, out)
	case "GET /tombstones":
		writeJSON(w, c.fake.Tombstones(c.sourceID))
	case "GET /heartbeats":
		writeJSON(w, c.fake.Heartbeats(c.sourceID))
	case "GET /media":
		writeJSON(w, c.fake.MediaFetches(c.sourceID))
	case "GET /hw":
		writeJSON(w, map[string]uint64{"hw": c.fake.HW(c.sourceID, r.URL.Query().Get("outbox"))})
	case "GET /piece":
		h, err := wire.ParseHash(r.URL.Query().Get("h"))
		if err != nil {
			http.Error(w, "hash", http.StatusBadRequest)
			return
		}
		kind, raw, ok := c.fake.Piece(h)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"kind": kind, "raw": base64.StdEncoding.EncodeToString(raw)})
	case "GET /stats":
		c.stats(w)
	case "POST /scan":
		c.scan(w, body)
	case "POST /unzstd":
		out, err := unzstd(bytes.NewReader(body))
		if err != nil {
			http.Error(w, "zstd", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(out)
	case "POST /fail_next":
		var q struct {
			Route       string `json:"route"`
			Status      int    `json:"status"`
			RetryAfterS int    `json:"retry_after_s"`
			Times       int    `json:"times"`
		}
		if json.Unmarshal(body, &q) != nil || q.Route == "" || q.Status == 0 {
			http.Error(w, "fail_next", http.StatusBadRequest)
			return
		}
		for i := 0; i < max(q.Times, 1); i++ {
			c.fake.FailNext(q.Route, q.Status, time.Duration(q.RetryAfterS)*time.Second)
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "POST /hold":
		var q struct {
			Route string `json:"route"`
		}
		if json.Unmarshal(body, &q) != nil || q.Route == "" {
			http.Error(w, "hold", http.StatusBadRequest)
			return
		}
		release := c.fake.Hold(q.Route)
		c.mu.Lock()
		c.n++
		id := fmt.Sprintf("h%d", c.n)
		c.holds[id] = release
		c.mu.Unlock()
		writeJSON(w, map[string]string{"id": id})
	case "POST /release":
		c.mu.Lock()
		for id, release := range c.holds {
			release()
			delete(c.holds, id)
		}
		c.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	case "POST /exact":
		var q struct {
			Rev       uint32   `json:"rev"`
			FPs       []string `json:"fps"`
			CallerFPs []string `json:"caller_fps"`
		}
		if json.Unmarshal(body, &q) != nil {
			http.Error(w, "exact", http.StatusBadRequest)
			return
		}
		parse := func(in []string) ([]wire.Hash, bool) {
			out := []wire.Hash{}
			for _, s := range in {
				h, err := wire.ParseHash(s)
				if err != nil {
					return nil, false
				}
				out = append(out, h)
			}
			return out, true
		}
		fps, ok1 := parse(q.FPs)
		callers, ok2 := parse(q.CallerFPs)
		if !ok1 || !ok2 {
			http.Error(w, "exact", http.StatusBadRequest)
			return
		}
		c.fake.SetExact(q.Rev, fps, callers)
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (c *control) refs() map[wire.Hash]bool {
	refs := map[wire.Hash]bool{}
	for _, rc := range c.fake.Envelopes(c.sourceID) {
		for _, h := range rc.Envelope.Refs {
			refs[h] = true
		}
	}
	return refs
}

func (c *control) stats(w http.ResponseWriter) {
	byStatus := map[string]int{}
	byKind := map[string]int{}
	keys := map[string]int{}
	for _, rc := range c.fake.Envelopes(c.sourceID) {
		byStatus[string(rc.Status)]++
		byKind[string(rc.Envelope.Kind)]++
		keys[rc.Envelope.Key]++
	}
	dupKeys := 0
	for _, n := range keys {
		if n > 1 {
			dupKeys++
		}
	}
	pieces, pieceBytes := 0, 0
	for h := range c.refs() {
		if _, raw, ok := c.fake.Piece(h); ok {
			pieces++
			pieceBytes += len(raw)
		}
	}
	bodies := c.fake.RequestBodies()
	total := 0
	for _, b := range bodies {
		total += len(b)
	}
	writeJSON(w, map[string]any{"by_status": byStatus, "by_kind": byKind, "keys": len(keys), "keys_received_twice": dupKeys,
		"pieces": pieces, "piece_bytes": pieceBytes, "request_bodies": len(bodies), "request_body_bytes": total,
		"heartbeats": len(c.fake.Heartbeats(c.sourceID)), "media_fetches": len(c.fake.MediaFetches(c.sourceID)),
		"tombstones": len(c.fake.Tombstones(c.sourceID))})
}

// isMediaFetch reports whether a request body is a POST /v1/media-fetch body, the one surface the
// unmasked media URL (canary M1) may reach.
func isMediaFetch(b []byte) bool {
	if len(b) == 0 || b[0] != '{' || len(b) > wire.MaxMediaFetchJSON {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	_, u := m["url"]
	_, e := m["expires_at"]
	_, rid := m["request_id"]
	return u && e && rid
}

// scan counts, per needle, the request bodies, media-fetch bodies, stored pieces and heartbeats that
// contain it. Needles are the encoded variants the harness computed; only counts come back.
func (c *control) scan(w http.ResponseWriter, body []byte) {
	var q struct {
		Needles map[string]string `json:"needles"`
	}
	if json.Unmarshal(body, &q) != nil {
		http.Error(w, "scan", http.StatusBadRequest)
		return
	}
	needles := map[string][]byte{}
	for name, v := range q.Needles {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) == 0 {
			http.Error(w, "scan needle", http.StatusBadRequest)
			return
		}
		needles[name] = b
	}
	bodies := c.fake.RequestBodies()
	var pieces [][]byte
	for h := range c.refs() {
		if _, raw, ok := c.fake.Piece(h); ok {
			pieces = append(pieces, raw)
		}
	}
	hbs, _ := json.Marshal(c.fake.Heartbeats(c.sourceID))
	type hit struct {
		Bodies      int `json:"bodies"`
		MediaBodies int `json:"media_bodies"`
		Pieces      int `json:"pieces"`
		Heartbeats  int `json:"heartbeats"`
	}
	out := map[string]hit{}
	for name, n := range needles {
		var h hit
		for _, b := range bodies {
			if bytes.Contains(b, n) {
				if isMediaFetch(b) {
					h.MediaBodies++
				} else {
					h.Bodies++
				}
			}
		}
		for _, p := range pieces {
			if bytes.Contains(p, n) {
				h.Pieces++
			}
		}
		if bytes.Contains(hbs, n) {
			h.Heartbeats = 1
		}
		out[name] = h
	}
	writeJSON(w, out)
}

// maskcheck reports, per input value, whether mask/v1 (with the given exact values and caller keys
// registered) changes it, and which rule ids fired. Values are never echoed.
func maskcheck() {
	var q struct {
		Values  []string `json:"values"`
		Exact   []string `json:"exact"`
		Callers []string `json:"callers"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&q); err != nil {
		fail("maskcheck: input")
	}
	cfg := mask.Config{ExactRev: 1}
	for _, v := range q.Exact {
		cfg.ExactFPs = append(cfg.ExactFPs, fingerprint.ExactFP(v))
	}
	for _, v := range q.Callers {
		h, err := wire.ParseHash(fingerprint.CallerScope(v))
		if err != nil {
			fail("maskcheck: caller")
		}
		cfg.CallerFPs = append(cfg.CallerFPs, h)
	}
	m, err := mask.New(cfg)
	if err != nil {
		fail("maskcheck: masker")
	}
	type result struct {
		Changed bool     `json:"changed"`
		Rules   []string `json:"rules"`
	}
	out := []result{}
	for _, v := range q.Values {
		masked, hits := m.String(v)
		r := result{Changed: masked != v, Rules: []string{}}
		for id := range hits {
			r.Rules = append(r.Rules, id)
		}
		out = append(out, r)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
