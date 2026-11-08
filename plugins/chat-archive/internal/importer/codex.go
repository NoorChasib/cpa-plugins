package importer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// codexFile is what the importer knows about one rollout before reading its records: ids from the first
// session_meta, the fork boundary, and which model call each line belongs to.
type codexFile struct {
	threadID  string
	sessionID string
	provider  string
	histStart int64            // subagent_history_start_ordinal; -1 when absent
	groups    map[int64]string // line number -> response id of its model call (closed, unambiguous calls only)
	covered   map[string]bool  // response ids CPA holds with content (session-coverage)
}

func (cf *codexFile) metadata(line int64) bool {
	if cf == nil || len(cf.covered) == 0 {
		return false
	}
	rid := cf.groups[line]
	return rid != "" && cf.covered[rid]
}

// readCodexHeader reads the first line of a rollout, which Codex always writes as session_meta.
func readCodexHeader(path string) (*codexFile, error) {
	cf := &codexFile{histStart: -1}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	line, err := bufio.NewReaderSize(fh, 64<<10).ReadBytes('\n')
	if err != nil && (err != io.EOF || len(line) == 0) {
		return cf, nil // no complete first line yet
	}
	if err == io.EOF {
		return cf, nil
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			ID         string       `json:"id"`
			SessionID  string       `json:"session_id"`
			Provider   string       `json:"model_provider"`
			HistoryOrd *json.Number `json:"subagent_history_start_ordinal"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "session_meta" {
		return cf, nil
	}
	cf.threadID, cf.sessionID, cf.provider = rec.Payload.ID, rec.Payload.SessionID, rec.Payload.Provider
	if rec.Payload.HistoryOrd != nil {
		if n, err := rec.Payload.HistoryOrd.Int64(); err == nil {
			cf.histStart = n
		}
	}
	return cf, nil
}

// Line classes for the per-call grouping.
const (
	clsNeutral = iota
	clsInput
	clsOutput
	clsUsage
)

// codexGroups assigns lines to model calls, so that records of a response CPA holds in full can go
// metadata-only. Each token_usage_record closes a call and names its response_id; the input items (sent
// with the call) and output items (its response items) since the previous token_usage_record belong to
// that response. Only input and output items join a call: every
// other record (item_completed, world_state, turn_context, task_*, compacted, the token_usage_record
// itself) belongs to no response and goes full. Lines after the last token_usage_record belong to no
// closed call. A call is assigned only when its shape is unambiguous: at least one input item, then at
// least one output item, no input item after an output item, and a response_id used by no other
// token_usage_record. Anything else stays unassigned and so goes full.
//
// Codex writes a call's output items, then (after item_completed records at most) its
// token_usage_record, then the next call's inputs. A probe of real rollouts found this in every call
// it could check and no input item between a call's outputs and its usage record. Calls without a
// usage record of their own, compaction calls and interrupted turns fail the shape check. So would a
// usage record written before its outputs, which leaves calls of only inputs or outputs first: an
// unexpected order falls back to full rather than dropping content.
func codexGroups(path string) (map[int64]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	r := bufio.NewReaderSize(fh, 1<<20)
	groups := map[int64]string{}
	byRID := map[string][]int64{}
	bad := map[string]bool{}
	var cur []int64
	sawIn, sawOut, inAfterOut := false, false, false
	for n := int64(1); ; n++ {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // the trailing partial line, if any, is not a record yet
		}
		cls, rid := classifyCodexLine(line)
		switch cls {
		case clsInput:
			cur = append(cur, n)
			sawIn = true
			if sawOut {
				inAfterOut = true
			}
		case clsOutput:
			cur = append(cur, n)
			sawOut = true
		case clsUsage:
			if rid == "" || !sawIn || !sawOut || inAfterOut || byRID[rid] != nil {
				if rid != "" {
					bad[rid] = true
				}
			} else {
				byRID[rid] = cur
			}
			cur, sawIn, sawOut, inAfterOut = nil, false, false, false
		}
	}
	for rid, lines := range byRID {
		if bad[rid] {
			continue
		}
		for _, l := range lines {
			groups[l] = rid
		}
	}
	return groups, nil
}

func classifyCodexLine(line []byte) (int, string) {
	var rec struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return clsNeutral, ""
	}
	var p struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		ResponseID string `json:"response_id"`
	}
	if len(rec.Payload) > 0 && rec.Payload[0] == '{' {
		_ = json.Unmarshal(rec.Payload, &p)
	}
	switch rec.Type {
	case "token_usage_record":
		return clsUsage, p.ResponseID
	case "response_item":
		switch {
		case p.Type == "message" && p.Role != "assistant",
			strings.HasSuffix(p.Type, "_output"),
			p.Type == "agent_message", p.Type == "additional_tools":
			return clsInput, ""
		}
		return clsOutput, ""
	}
	return clsNeutral, ""
}

// codexRecord builds one rollout record.
func (b *builder) codexRecord(f fileRef, cf *codexFile, line []byte, lineNo int64) (*built, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil || top == nil {
		return nil, errSkip
	}
	typ := jsonStr(top["type"])
	if typ == "session_meta" && lineNo > 1 {
		return nil, errSkip // a forked rollout's second (parent) session_meta
	}
	ordinal := lineNo - 1
	if n, ok := jsonInt(top["ordinal"]); ok {
		ordinal = n
	}
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(top["payload"], &payload)
	rtype := typ
	if pt := jsonStr(payload["type"]); pt != "" {
		rtype = typ + "/" + pt
	}
	ts := parseTS(top["timestamp"])
	rec := wire.ImportRecord{
		Client: ClientCodex, FileRole: RoleCodexRollout, SessionID: cf.sessionID, ThreadID: cf.threadID,
		RecordType: rtype, RecordTS: ts, Mode: ModeFull,
		// The first session_meta is the fork's own identity record, never inherited parent history.
		Inherited: cf.histStart >= 0 && ordinal < cf.histStart && lineNo > 1,
	}
	a := b.newAcc()
	var frame json.RawMessage
	if typ == "response_item" && payload != nil {
		arr := append(append([]byte{'['}, top["payload"]...), ']')
		sp, err := canon.SplitMessages(b.ctx, canon.FormatOpenAIResponses, arr, b.splitOpts())
		if err != nil {
			return nil, err
		}
		a.addSplit(sp)
		delete(top, "payload")
		frame, err = b.frame(top)
		if err != nil {
			return nil, err
		}
	} else {
		v, err := decodeAny(line)
		if err != nil {
			return nil, errSkip
		}
		if typ == "event_msg" && rtype == "event_msg/item_completed" {
			dedupeCommandOutput(v)
		}
		if v, err = a.extractLeaves(v, nil); err != nil {
			return nil, err
		}
		frame, err = b.frame(v)
		if err != nil {
			return nil, err
		}
	}
	rec.Frame = frame
	if cf.metadata(lineNo) && a.metadataOnly() {
		rec.Mode = ModeMetadata
	}
	key := func(sha string) string { return wire.CodexKey(keyPart(cf.threadID), ordinal, sha) }
	return b.finishRecord(rec, a, key, createdAt(ts, f))
}

// dedupeCommandOutput drops a CommandExecution's stdout and formatted_output where they equal
// aggregated_output, which keeps every output once.
func dedupeCommandOutput(v any) {
	top, _ := v.(map[string]any)
	payload, _ := top["payload"].(map[string]any)
	item, _ := payload["item"].(map[string]any)
	if item == nil || item["type"] != "CommandExecution" {
		return
	}
	agg, ok := item["aggregated_output"].(string)
	if !ok {
		return
	}
	for _, k := range []string{"stdout", "formatted_output"} {
		if s, ok := item[k].(string); ok && s == agg {
			delete(item, k)
		}
	}
}

func jsonInt(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == '"' {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}
