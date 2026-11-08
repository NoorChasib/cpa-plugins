package importer

import (
	"encoding/json"
	"strconv"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// toolUseResultKeep is the structured part of toolUseResult kept in ImportRecord.Extra.
// Everything else in toolUseResult repeats the tool_result text and is dropped.
var toolUseResultKeep = []string{
	"interrupted", "returnCodeInterpretation", "filePath", "structuredPatch",
	// agent stats
	"agentId", "agentType", "resolvedModel", "status", "totalDurationMs", "totalTokens", "totalToolUseCount",
	"usage", "toolStats", "toolsUsed",
	// Workflow
	"runId", "taskId", "taskType", "scriptPath", "transcriptDir", "workflowName",
	// Bash
	"gitOperation", "persistedOutputPath", "persistedOutputSize", "backgroundTaskId", "timedOutAfterMs",
}

// claudeRecord builds one Claude Code record of a main, subagent or workflow-agent file, or of a
// workflow journal.
func (b *builder) claudeRecord(f fileRef, line []byte) (*built, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil || top == nil {
		return nil, errSkip
	}
	if f.Role == RoleWorkflowJournal {
		return b.journalRecord(f, top)
	}
	typ := jsonStr(top["type"])
	uuid := jsonStr(top["uuid"])
	sid := jsonStr(top["sessionId"])
	if sid == "" {
		sid = f.SessionID // file-history-* records carry no sessionId; the file's session keys them
	}
	agent := jsonStr(top["agentId"])
	if agent == "" {
		agent = f.AgentID
	}
	ts := parseTS(top["timestamp"])
	rec := wire.ImportRecord{
		Client: ClientClaudeCode, FileRole: f.Role, SessionID: sid, AgentID: agent,
		RecordUUID: uuid, RecordType: typ, RecordTS: ts, Mode: ModeFull,
	}
	a := b.newAcc()

	wti := top["wireToolInputs"]
	delete(top, "wireToolInputs")
	delete(top, "wireIngestContext")

	// Message content: the Claude block rules, tool_use.input from wireToolInputs (so pieces hash equal to
	// CPA's). api-request-blob keeps only system-role content, the only copy of the system prompt; user
	// and assistant blob bodies are dropped.
	if raw, ok := top["message"]; ok {
		var msg map[string]json.RawMessage
		if json.Unmarshal(raw, &msg) == nil && msg != nil {
			if content, ok := msg["content"]; ok {
				delete(msg, "content")
				role := msg["role"]
				if typ != "api-request-blob" || jsonStr(role) == "system" {
					if err := b.claudeContent(a, role, content, wti); err != nil {
						return nil, err
					}
				}
				nm, err := json.Marshal(msg)
				if err != nil {
					return nil, err
				}
				top["message"] = nm
			}
		}
	}

	if tur, ok := top["toolUseResult"]; ok {
		delete(top, "toolUseResult")
		h, err := b.toolUseResultExtra(a, tur)
		if err != nil {
			return nil, err
		}
		rec.Extra = h
	}

	var err error
	switch typ {
	case "api-request-shape":
		err = b.moveArrays(a, top, "shape", "system", "tools")
	case "attachment":
		err = b.attachment(a, top)
	case "queue-operation":
		err = b.moveField(a, top, "content")
	case "last-prompt":
		err = b.moveField(a, top, "lastPrompt")
	}
	if err != nil {
		return nil, err
	}

	frame, err := b.claudeFrame(a, top)
	if err != nil {
		return nil, err
	}
	rec.Frame = frame
	key := func(sha string) string {
		if uuid != "" {
			return wire.ClaudeUUIDKey(keyPart(sid), keyPart(uuid), sha)
		}
		return wire.ClaudeNoUUIDKey(keyPart(sid), keyPart(typ), sha)
	}
	return b.finishRecord(rec, a, key, createdAt(ts, f))
}

// claudeFrame encodes the frame; with --split its large string leaves move to text pieces first.
func (b *builder) claudeFrame(a *acc, top map[string]json.RawMessage) (json.RawMessage, error) {
	if !b.split {
		return b.frame(top)
	}
	raw, err := json.Marshal(top)
	if err != nil {
		return nil, err
	}
	v, err := decodeAny(raw)
	if err != nil {
		return nil, err
	}
	if v, err = a.extractLeaves(v, nil); err != nil {
		return nil, err
	}
	return b.frame(v)
}

// claudeContent splits message.content with the Claude block rules as one {role, content} message.
func (b *builder) claudeContent(a *acc, role, content, wti json.RawMessage) error {
	if len(wti) > 0 {
		content = applyWireToolInputs(content, wti)
	}
	m := map[string]json.RawMessage{"content": content}
	if len(role) > 0 {
		m["role"] = role
	}
	arr, err := json.Marshal([]any{m})
	if err != nil {
		return err
	}
	sp, err := canon.SplitMessages(b.ctx, canon.FormatClaude, arr, b.splitOpts())
	if err != nil {
		return err
	}
	a.addSplit(sp)
	return nil
}

func (b *builder) splitOpts() canon.Options {
	return canon.Options{Masker: b.m, MaxAttachment: b.limits.Attachment, Memo: b.memo}
}

// applyWireToolInputs replaces each tool_use.input by wireToolInputs[<tool_use id>] when present, so
// transcript pieces hash equal to CPA's wire pieces.
func applyWireToolInputs(content, wti json.RawMessage) json.RawMessage {
	var inputs map[string]json.RawMessage
	if json.Unmarshal(wti, &inputs) != nil || len(inputs) == 0 {
		return content
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return content
	}
	changed := false
	for i, raw := range blocks {
		var blk map[string]json.RawMessage
		if json.Unmarshal(raw, &blk) != nil || jsonStr(blk["type"]) != "tool_use" {
			continue
		}
		in, ok := inputs[jsonStr(blk["id"])]
		if !ok {
			continue
		}
		blk["input"] = in
		nb, err := json.Marshal(blk)
		if err != nil {
			continue
		}
		blocks[i], changed = nb, true
	}
	if !changed {
		return content
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return content
	}
	return out
}

// toolUseResultExtra keeps the structured fields of a dict toolUseResult as one json piece.
func (b *builder) toolUseResultExtra(a *acc, tur json.RawMessage) (*wire.Hash, error) {
	var m map[string]json.RawMessage
	if json.Unmarshal(tur, &m) != nil || m == nil {
		return nil, nil // a string or list result only repeats the tool_result text
	}
	kept := map[string]json.RawMessage{}
	for _, k := range toolUseResultKeep {
		if v, ok := m[k]; ok {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	p, err := b.jsonPiece(raw)
	if err != nil {
		return nil, err
	}
	h := a.addPiece(p)
	return &h, nil
}

// moveArrays moves each element of top[obj][k] (for each k) to its own json piece.
func (b *builder) moveArrays(a *acc, top map[string]json.RawMessage, obj string, keys ...string) error {
	var o map[string]json.RawMessage
	if json.Unmarshal(top[obj], &o) != nil || o == nil {
		return nil
	}
	for _, k := range keys {
		var arr []json.RawMessage
		if json.Unmarshal(o[k], &arr) != nil {
			continue
		}
		for i, e := range arr {
			if err := a.pathJSON(b.pointer(obj, k, strconv.Itoa(i)), e); err != nil {
				return err
			}
		}
		delete(o, k)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	top[obj] = raw
	return nil
}

// moveField moves top[k] to a text piece (a string) or a json piece (anything else but null).
func (b *builder) moveField(a *acc, top map[string]json.RawMessage, k string) error {
	raw, ok := top[k]
	if !ok || string(raw) == "null" {
		return nil
	}
	var err error
	if len(raw) > 0 && raw[0] == '"' {
		err = a.pathText(b.pointer(k), jsonStr(raw))
	} else {
		err = a.pathJSON(b.pointer(k), raw)
	}
	if err != nil {
		return err
	}
	delete(top, k)
	return nil
}

// attachment applies the attachment.* rule: prompt_snapshot's systemPrompt[] and tools[]
// entries are one json piece each, the rest stays in the frame; any other attachment is one json
// piece and the frame keeps only its type.
func (b *builder) attachment(a *acc, top map[string]json.RawMessage) error {
	raw, ok := top["attachment"]
	if !ok {
		return nil
	}
	var att map[string]json.RawMessage
	if json.Unmarshal(raw, &att) != nil || att == nil {
		return b.moveField(a, top, "attachment")
	}
	t := att["type"]
	if jsonStr(t) == "prompt_snapshot" {
		return b.moveArrays(a, top, "attachment", "systemPrompt", "tools")
	}
	if err := a.pathJSON(b.pointer("attachment"), raw); err != nil {
		return err
	}
	if len(t) > 0 {
		top["attachment"], _ = json.Marshal(map[string]json.RawMessage{"type": t})
	} else {
		delete(top, "attachment")
	}
	return nil
}

// journalRecord builds a workflow journal record: key cc/j/…, result.result as a json piece.
func (b *builder) journalRecord(f fileRef, top map[string]json.RawMessage) (*built, error) {
	typ := jsonStr(top["type"])
	agent := jsonStr(top["agentId"])
	ts := parseTS(top["timestamp"])
	rec := wire.ImportRecord{
		Client: ClientClaudeCode, FileRole: RoleWorkflowJournal, SessionID: f.SessionID, AgentID: agent,
		RecordType: typ, RecordTS: ts, Mode: ModeFull,
	}
	a := b.newAcc()
	if typ == "result" {
		if raw, ok := top["result"]; ok && string(raw) != "null" {
			if err := a.pathJSON(b.pointer("result"), raw); err != nil {
				return nil, err
			}
			delete(top, "result")
		}
	}
	frame, err := b.claudeFrame(a, top)
	if err != nil {
		return nil, err
	}
	rec.Frame = frame
	key := func(sha string) string {
		ag := agent
		if ag != "" {
			ag = keyPart(ag)
		}
		return wire.JournalKey(keyPart(f.RunID), ag, keyPart(typ), sha)
	}
	return b.finishRecord(rec, a, key, createdAt(ts, f))
}
