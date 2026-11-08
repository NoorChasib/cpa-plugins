package stream

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Terminal statuses (wire.ExchangeResponse.TerminalStatus).
const (
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusFailed     = "failed"
	StatusDone       = "done"
	StatusSuperseded = "superseded"
	StatusCanceled   = "canceled"
)

// Response is one reassembled response: Body is the equivalent non-stream response object.
type Response struct {
	ResponseID       string
	PrevResponseID   string
	Body             []byte
	TerminalStatus   string
	IncompleteReason string
	TerminalAt       time.Time // zero when no terminal event arrived
	Events           []wire.StreamEvent
	Flags            []string // wire.FlagInputParseError
}

// Reassembler rebuilds responses from events of one RequestID.
type Reassembler interface {
	// Feed applies one event and returns the responses it completed.
	Feed(ev Event, at time.Time) []Response
	// Flush closes every open response with status (failed, canceled or incomplete; chat and
	// Gemini report completed when a finish reason arrived) and returns them.
	Flush(status string, at time.Time) []Response
}

// New returns the reassembler for a response protocol (SourceFormat on stream chunks).
func New(sourceFormat string) Reassembler {
	switch sourceFormat {
	case "claude":
		return &claude{}
	case "openai":
		return &chat{}
	case "openai-response", "codex":
		return &responses{}
	case "gemini":
		return &gemini{}
	case "interactions":
		return &generic{interactions: true}
	default:
		return &generic{}
	}
}

type obj = map[string]any

func decode(b []byte) (obj, bool) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var o obj
	if d.Decode(&o) != nil || o == nil {
		return nil, false
	}
	return o, true
}

// str returns o[k] as a string: a string value, or the text an accumulation builder holds.
func str(o obj, k string) string {
	switch v := o[k].(type) {
	case string:
		return v
	case *strings.Builder:
		return v.String() // no copy
	}
	return ""
}

// isText reports whether v is a string or an accumulation builder.
func isText(v any) bool {
	switch v.(type) {
	case string, *strings.Builder:
		return true
	}
	return false
}

func sub(o obj, k string) obj {
	m, _ := o[k].(map[string]any)
	return m
}

func list(o obj, k string) []any {
	l, _ := o[k].([]any)
	return l
}

func intOf(v any, def int) int {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case float64:
		return int(n)
	}
	return def
}

func typeOf(ev Event, o obj) string {
	if t := str(o, "type"); t != "" {
		return t
	}
	return ev.Name
}

// marshal materializes every accumulation builder in o, then encodes it.
func marshal(o obj) []byte {
	materialize(o)
	b, err := json.Marshal(o)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// appendStr appends a delta to the text at o[k]. Text deltas accumulate in a *strings.Builder
// (amortized linear in the total text; concatenating strings would copy the whole text on every
// delta), which marshal turns back into a string. A nil o is ignored.
func appendStr(o obj, k, add string) {
	if o == nil || add == "" {
		return
	}
	if b, ok := o[k].(*strings.Builder); ok {
		b.WriteString(add)
		return
	}
	b := &strings.Builder{}
	if s, ok := o[k].(string); ok {
		b.WriteString(s)
	}
	b.WriteString(add)
	o[k] = b
}

// materialize replaces every accumulation builder under v by its string, in place.
func materialize(v any) any {
	switch x := v.(type) {
	case *strings.Builder:
		return x.String()
	case map[string]any:
		for k, e := range x {
			x[k] = materialize(e)
		}
	case []any:
		for i, e := range x {
			x[i] = materialize(e)
		}
	}
	return v
}

// maxParts bounds content_index and summary_index. Those index a slice, so an index taken from
// the upstream as is would allocate in proportion to it (or panic when negative); real responses
// carry a handful of parts per item.
const maxParts = 1024

// ---- Claude Messages ----

type claude struct {
	msg     obj
	blocks  map[int]obj
	partial map[int]*strings.Builder
	flags   []string
	failed  bool
	open    bool
}

func (c *claude) reset() {
	c.msg, c.blocks, c.partial, c.flags, c.failed, c.open = obj{}, map[int]obj{}, map[int]*strings.Builder{}, nil, false, true
}

func (c *claude) Feed(ev Event, at time.Time) []Response {
	o, ok := decode(ev.Data)
	if !ok {
		return nil
	}
	if !c.open {
		c.reset()
	}
	switch typeOf(ev, o) {
	case "message_start":
		if m := sub(o, "message"); m != nil {
			c.msg = m
		}
	case "content_block_start":
		i := intOf(o["index"], len(c.blocks))
		if b := sub(o, "content_block"); b != nil {
			c.blocks[i] = b
		}
	case "content_block_delta":
		i := intOf(o["index"], 0)
		b := c.blocks[i]
		if b == nil {
			b = obj{}
			c.blocks[i] = b
		}
		d := sub(o, "delta")
		switch str(d, "type") {
		case "text_delta":
			appendStr(b, "text", str(d, "text"))
		case "thinking_delta":
			appendStr(b, "thinking", str(d, "thinking"))
		case "signature_delta":
			appendStr(b, "signature", str(d, "signature"))
		case "input_json_delta":
			sb := c.partial[i]
			if sb == nil {
				sb = &strings.Builder{}
				c.partial[i] = sb
			}
			sb.WriteString(str(d, "partial_json"))
		case "citations_delta":
			b["citations"] = append(list(b, "citations"), d["citation"])
		default:
			for k, v := range d {
				if s, ok := v.(string); ok && k != "type" {
					appendStr(b, k, s)
				}
			}
		}
	case "content_block_stop":
		i := intOf(o["index"], 0)
		c.closeBlock(i)
	case "message_delta":
		for k, v := range sub(o, "delta") {
			c.msg[k] = v
		}
		if u := sub(o, "usage"); u != nil {
			mu := sub(c.msg, "usage")
			if mu == nil {
				mu = obj{}
			}
			for k, v := range u {
				mu[k] = v
			}
			c.msg["usage"] = mu
		}
	case "message_stop":
		return []Response{c.finish(StatusCompleted, at, true)}
	case "error":
		c.msg["error"] = o["error"]
		c.failed = true
		return []Response{c.finish(StatusFailed, at, true)}
	case "message":
		c.msg = o
		c.blocks = map[int]obj{}
		for i, b := range list(o, "content") {
			if m, ok := b.(map[string]any); ok {
				c.blocks[i] = m
			}
		}
		return []Response{c.finish(StatusCompleted, at, true)}
	}
	return nil
}

func (c *claude) closeBlock(i int) {
	sb := c.partial[i]
	if sb == nil {
		return
	}
	b := c.blocks[i]
	if b == nil {
		b = obj{}
		c.blocks[i] = b
	}
	raw := sb.String()
	if strings.TrimSpace(raw) == "" {
		b["input"] = obj{}
	} else if v, ok := decodeAny([]byte(raw)); ok {
		b["input"] = v
	} else {
		b["input"] = raw
		c.flags = append(c.flags, wire.FlagInputParseError)
	}
	delete(c.partial, i)
}

func decodeAny(b []byte) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if d.Decode(&v) != nil || d.More() {
		return nil, false
	}
	return v, true
}

func (c *claude) finish(status string, at time.Time, terminal bool) Response {
	for i := range c.partial {
		c.closeBlock(i)
	}
	idx := make([]int, 0, len(c.blocks))
	for i := range c.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	content := make([]any, 0, len(idx))
	for _, i := range idx {
		content = append(content, c.blocks[i])
	}
	c.msg["content"] = content
	if _, ok := c.msg["type"]; !ok {
		c.msg["type"] = "message"
	}
	r := Response{ResponseID: str(c.msg, "id"), Body: marshal(c.msg), TerminalStatus: status, Flags: c.flags}
	if terminal {
		r.TerminalAt = at
	}
	c.open = false
	return r
}

func (c *claude) Flush(status string, at time.Time) []Response {
	if !c.open {
		return nil
	}
	if status == StatusCompleted {
		status = StatusIncomplete // no message_stop before the stream ended
	}
	return []Response{c.finish(status, at, false)}
}

// ---- OpenAI chat completions ----

type chat struct {
	top     obj
	choices map[int]*chatChoice
	failed  bool
	open    bool
}

type chatChoice struct {
	msg     obj
	tools   map[int]obj
	details []obj
	finish  any
	extra   obj
}

func (c *chat) Feed(ev Event, at time.Time) []Response {
	if bytes.Equal(ev.Data, done) {
		if !c.open {
			return nil
		}
		return []Response{c.finish(StatusCompleted, at, true)}
	}
	o, ok := decode(ev.Data)
	if !ok {
		return nil
	}
	if !c.open {
		c.top, c.choices, c.failed, c.open = obj{}, map[int]*chatChoice{}, false, true
	}
	if e, ok := o["error"]; ok && o["choices"] == nil {
		c.top["error"] = e
		return []Response{c.finish(StatusFailed, at, true)}
	}
	for k, v := range o {
		switch k {
		case "choices":
		case "object":
		default:
			c.top[k] = v
		}
	}
	for _, raw := range list(o, "choices") {
		ch, _ := raw.(map[string]any)
		if ch == nil {
			continue
		}
		i := intOf(ch["index"], 0)
		cc := c.choices[i]
		if cc == nil {
			cc = &chatChoice{msg: obj{}, tools: map[int]obj{}, extra: obj{}}
			c.choices[i] = cc
		}
		if fr, ok := ch["finish_reason"]; ok && fr != nil {
			cc.finish = fr
		}
		for k, v := range ch {
			if k != "index" && k != "delta" && k != "finish_reason" && k != "message" {
				cc.extra[k] = v
			}
		}
		d := sub(ch, "delta")
		if d == nil {
			d = sub(ch, "message")
		}
		for k, v := range d {
			switch k {
			case "content", "reasoning_content", "reasoning", "refusal":
				if s, ok := v.(string); ok {
					appendStr(cc.msg, k, s)
				} else if _, has := cc.msg[k]; !has {
					cc.msg[k] = v
				}
			case "role":
				if s, ok := v.(string); ok && s != "" {
					cc.msg["role"] = s
				}
			case "tool_calls":
				for _, t := range listAny(v) {
					tc, _ := t.(map[string]any)
					if tc == nil {
						continue
					}
					ti := intOf(tc["index"], len(cc.tools))
					cur := cc.tools[ti]
					if cur == nil {
						cur = obj{"function": obj{}}
						cc.tools[ti] = cur
					}
					for _, f := range []string{"id", "type"} {
						if s := str(tc, f); s != "" && str(cur, f) == "" {
							cur[f] = s
						}
					}
					fn := sub(tc, "function")
					cf := sub(cur, "function")
					if s := str(fn, "name"); s != "" && str(cf, "name") == "" {
						cf["name"] = s
					}
					appendStr(cf, "arguments", str(fn, "arguments"))
					for k2, v2 := range tc {
						if k2 != "index" && k2 != "id" && k2 != "type" && k2 != "function" {
							cur[k2] = v2
						}
					}
				}
			case "reasoning_details":
				for _, rd := range listAny(v) {
					m, _ := rd.(map[string]any)
					if m == nil {
						continue
					}
					cc.mergeDetail(m)
				}
			case "images", "annotations":
				cc.msg[k] = append(list(cc.msg, k), listAny(v)...)
			default:
				cc.msg[k] = v
			}
		}
	}
	return nil
}

func listAny(v any) []any {
	l, _ := v.([]any)
	return l
}

func (cc *chatChoice) mergeDetail(m obj) {
	idx, typ := intOf(m["index"], -1), str(m, "type")
	for _, d := range cc.details {
		if intOf(d["index"], -1) == idx && str(d, "type") == typ && idx >= 0 {
			for k, v := range m {
				switch k {
				case "text", "summary":
					if s, ok := v.(string); ok {
						appendStr(d, k, s)
						continue
					}
					d[k] = v
				case "signature":
					if v != nil {
						d[k] = v
					}
				default:
					d[k] = v
				}
			}
			return
		}
	}
	cp := obj{}
	for k, v := range m {
		cp[k] = v
	}
	cc.details = append(cc.details, cp)
}

func (c *chat) finish(status string, at time.Time, terminal bool) Response {
	idx := make([]int, 0, len(c.choices))
	for i := range c.choices {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	choices := make([]any, 0, len(idx))
	anyFinish := false
	for _, i := range idx {
		cc := c.choices[i]
		if len(cc.tools) > 0 {
			ti := make([]int, 0, len(cc.tools))
			for k := range cc.tools {
				ti = append(ti, k)
			}
			sort.Ints(ti)
			tools := make([]any, 0, len(ti))
			for _, k := range ti {
				tools = append(tools, cc.tools[k])
			}
			cc.msg["tool_calls"] = tools
		}
		if len(cc.details) > 0 {
			ds := make([]any, len(cc.details))
			for k, d := range cc.details {
				ds[k] = d
			}
			cc.msg["reasoning_details"] = ds
		}
		if _, ok := cc.msg["role"]; !ok {
			cc.msg["role"] = "assistant"
		}
		ch := obj{"index": json.Number(itoa(i)), "message": cc.msg, "finish_reason": cc.finish}
		for k, v := range cc.extra {
			ch[k] = v
		}
		if cc.finish != nil {
			anyFinish = true
		}
		choices = append(choices, ch)
	}
	if status == StatusCompleted && !anyFinish && !terminal {
		status = StatusIncomplete
	}
	c.top["object"] = "chat.completion"
	c.top["choices"] = choices
	r := Response{ResponseID: str(c.top, "id"), Body: marshal(c.top), TerminalStatus: status}
	if terminal {
		r.TerminalAt = at
	}
	c.open = false
	return r
}

func (c *chat) Flush(status string, at time.Time) []Response {
	if !c.open {
		return nil
	}
	return []Response{c.finish(status, at, false)}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// ---- OpenAI Responses ----

type responses struct {
	cur  *respState
	prev string // id of the last closed response
}

type respState struct {
	base     obj
	items    map[int]obj
	fallback []obj
	byID     map[string]obj
	doneSeen bool
	events   []wire.StreamEvent
	partials []any
	metaEvs  []any
	created  bool
}

func newRespState() *respState {
	return &respState{base: obj{}, items: map[int]obj{}, byID: map[string]obj{}}
}

var droppedResponseEvents = map[string]bool{
	"codex.rate_limits": true, "response.subscription_usage": true, "ping": true,
}

func (r *responses) Feed(ev Event, at time.Time) []Response {
	o, ok := decode(ev.Data)
	if !ok {
		return nil
	}
	t := typeOf(ev, o)
	if droppedResponseEvents[t] || strings.HasPrefix(t, "responsesapi.") {
		return nil
	}
	var out []Response
	switch t {
	case "response.created", "response.in_progress", "response.queued":
		resp := sub(o, "response")
		id := str(resp, "id")
		if r.cur != nil && r.cur.created && id != "" && str(r.cur.base, "id") != "" && str(r.cur.base, "id") != id {
			out = append(out, r.close(StatusSuperseded, at, false))
		}
		if r.cur == nil {
			r.cur = newRespState()
		}
		r.cur.created = true
		for k, v := range resp {
			if k != "output" {
				r.cur.base[k] = v
			}
		}
		return out
	}
	if r.cur == nil {
		r.cur = newRespState()
	}
	s := r.cur
	switch t {
	case "response.output_item.added", "response.output_item.done":
		it := sub(o, "item")
		if it == nil {
			return nil
		}
		if oi, ok := o["output_index"]; ok {
			s.items[intOf(oi, 0)] = it
		} else {
			s.fallback = append(s.fallback, it)
		}
		if id := str(it, "id"); id != "" {
			s.byID[id] = it
		}
		if t == "response.output_item.done" {
			s.doneSeen = true
		}
	case "response.content_part.added", "response.content_part.done":
		if it := s.item(o); it != nil {
			setIndexed(it, "content", intOf(o["content_index"], 0), sub(o, "part"))
		}
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta":
		if it := s.item(o); it != nil {
			field := "text"
			if t == "response.refusal.delta" {
				field = "refusal"
			}
			p := indexed(it, "content", intOf(o["content_index"], 0))
			appendStr(p, field, str(o, "delta"))
		}
	case "response.output_text.done", "response.reasoning_text.done":
		if it := s.item(o); it != nil {
			if v, ok := o["text"].(string); ok {
				if p := indexed(it, "content", intOf(o["content_index"], 0)); p != nil {
					p["text"] = v
				}
			}
		}
	case "response.output_text.annotation.added":
		if it := s.item(o); it != nil {
			if p := indexed(it, "content", intOf(o["content_index"], 0)); p != nil {
				p["annotations"] = append(list(p, "annotations"), o["annotation"])
			}
		}
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if it := s.item(o); it != nil {
			setIndexed(it, "summary", intOf(o["summary_index"], 0), sub(o, "part"))
		}
	case "response.reasoning_summary_text.delta":
		if it := s.item(o); it != nil {
			appendStr(indexed(it, "summary", intOf(o["summary_index"], 0)), "text", str(o, "delta"))
		}
	case "response.reasoning_summary_text.done":
		if it := s.item(o); it != nil {
			if v, ok := o["text"].(string); ok {
				if p := indexed(it, "summary", intOf(o["summary_index"], 0)); p != nil {
					p["text"] = v
				}
			}
		}
	case "response.function_call_arguments.delta", "response.mcp_call_arguments.delta":
		if it := s.item(o); it != nil {
			appendStr(it, "arguments", str(o, "delta"))
		}
	case "response.function_call_arguments.done", "response.mcp_call_arguments.done":
		if it := s.item(o); it != nil {
			if v, ok := o["arguments"].(string); ok {
				it["arguments"] = v
			}
		}
	case "response.custom_tool_call_input.delta":
		if it := s.item(o); it != nil {
			appendStr(it, "input", str(o, "delta"))
		}
	case "response.custom_tool_call_input.done":
		if it := s.item(o); it != nil {
			if v, ok := o["input"].(string); ok {
				it["input"] = v
			}
		}
	case "response.image_generation_call.partial_image":
		b64 := str(o, "partial_image_b64")
		sum, n := hashBase64(b64)
		s.partials = append(s.partials, obj{
			"output_index": o["output_index"], "partial_image_index": o["partial_image_index"],
			"sha256": sum, "len": json.Number(itoa(n)),
		})
	case "response.metadata":
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ks := make([]any, len(keys))
		for i, k := range keys {
			ks[i] = k
		}
		s.metaEvs = append(s.metaEvs, obj{"type": t, "keys": ks})
	case "response.steer.accepted", "response.steer.failed", "response.steer.pending":
		st := sub(o, "steer")
		s.events = append(s.events, wire.StreamEvent{Type: t, At: at.UTC(), SteerID: str(st, "id"), PrevResponseID: str(st, "previous_response_id")})
	case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
		resp := sub(o, "response")
		if t == "error" {
			resp = obj{"error": o["error"]}
			if resp["error"] == nil {
				resp["error"] = obj{"code": o["code"], "message": o["message"]}
			}
		}
		for _, k := range []string{"id", "status", "incomplete_details", "error", "usage", "model", "service_tier"} {
			if v, ok := resp[k]; ok {
				s.base[k] = v
			}
		}
		if !s.doneSeen && len(s.items) == 0 && len(s.fallback) == 0 {
			for i, it := range list(resp, "output") {
				if m, ok := it.(map[string]any); ok {
					s.items[i] = m
				}
			}
		}
		status := map[string]string{
			"response.completed": StatusCompleted, "response.done": StatusDone,
			"response.incomplete": StatusIncomplete, "response.failed": StatusFailed, "error": StatusFailed,
		}[t]
		return append(out, r.close(status, at, true))
	}
	return out
}

func (s *respState) item(o obj) obj {
	if oi, ok := o["output_index"]; ok {
		if it := s.items[intOf(oi, -1)]; it != nil {
			return it
		}
	}
	if id := str(o, "item_id"); id != "" {
		if it := s.byID[id]; it != nil {
			return it
		}
	}
	if oi, ok := o["output_index"]; ok {
		it := obj{}
		s.items[intOf(oi, 0)] = it
		return it
	}
	return nil
}

// indexed returns part i of it[k], growing the list as needed; nil (the event is ignored) when i
// is outside [0, maxParts).
func indexed(it obj, k string, i int) obj {
	if i < 0 || i >= maxParts {
		return nil
	}
	l := list(it, k)
	for len(l) <= i {
		l = append(l, obj{})
	}
	it[k] = l
	m, ok := l[i].(map[string]any)
	if !ok {
		m = obj{}
		l[i] = m
	}
	return m
}

func setIndexed(it obj, k string, i int, v obj) {
	if v == nil || i < 0 || i >= maxParts {
		return
	}
	l := list(it, k)
	for len(l) <= i {
		l = append(l, obj{})
	}
	l[i] = v
	it[k] = l
}

func (r *responses) close(status string, at time.Time, terminal bool) Response {
	s := r.cur
	r.cur = nil
	idx := make([]int, 0, len(s.items))
	for i := range s.items {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	output := make([]any, 0, len(idx)+len(s.fallback))
	for _, i := range idx {
		output = append(output, s.items[i])
	}
	for _, it := range s.fallback {
		output = append(output, it)
	}
	s.base["output"] = output
	if _, ok := s.base["object"]; !ok {
		s.base["object"] = "response"
	}
	if len(s.partials) > 0 {
		s.base["partial_images"] = s.partials
	}
	if len(s.metaEvs) > 0 {
		s.base["stream_metadata"] = s.metaEvs
	}
	resp := Response{
		ResponseID:     str(s.base, "id"),
		PrevResponseID: str(s.base, "previous_response_id"),
		Body:           marshal(s.base),
		TerminalStatus: status,
		Events:         s.events,
	}
	if d := sub(s.base, "incomplete_details"); d != nil {
		resp.IncompleteReason = str(d, "reason")
	}
	if terminal {
		resp.TerminalAt = at
	}
	r.prev = resp.ResponseID
	return resp
}

func (r *responses) Flush(status string, at time.Time) []Response {
	if r.cur == nil {
		return nil
	}
	if status == StatusCompleted {
		status = StatusIncomplete
	}
	return []Response{r.close(status, at, false)}
}

func hashBase64(s string) (string, int) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b = []byte(s)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), len(b)
}

// ---- Gemini ----

type gemini struct {
	top   obj
	cands map[int]*geminiCand
	open  bool
}

type geminiCand struct {
	c      obj
	role   any
	parts  []obj
	finish bool
	chunks []any
}

func (g *gemini) Feed(ev Event, at time.Time) []Response {
	o, ok := decode(ev.Data)
	if !ok {
		return nil
	}
	if !g.open {
		g.top, g.cands, g.open = obj{}, map[int]*geminiCand{}, true
	}
	if e, ok := o["error"]; ok && o["candidates"] == nil {
		g.top["error"] = e
		return []Response{g.finish(StatusFailed, at, true)}
	}
	for k, v := range o {
		if k != "candidates" {
			g.top[k] = v
		}
	}
	for n, raw := range list(o, "candidates") {
		c, _ := raw.(map[string]any)
		if c == nil {
			continue
		}
		i := intOf(c["index"], n)
		gc := g.cands[i]
		if gc == nil {
			gc = &geminiCand{c: obj{}}
			g.cands[i] = gc
		}
		for k, v := range c {
			switch k {
			case "content":
			case "groundingMetadata":
				gm, _ := v.(map[string]any)
				gc.chunks = append(gc.chunks, list(gm, "groundingChunks")...)
				gc.c[k] = v
			default:
				gc.c[k] = v
			}
		}
		if _, ok := c["finishReason"]; ok {
			gc.finish = true
		}
		content := sub(c, "content")
		if r, ok := content["role"]; ok {
			gc.role = r
		}
		for _, p := range list(content, "parts") {
			part, _ := p.(map[string]any)
			if part == nil {
				continue
			}
			gc.addPart(part)
		}
	}
	return nil
}

func (gc *geminiCand) addPart(p obj) {
	if t, ok := p["text"].(string); ok && len(gc.parts) > 0 {
		last := gc.parts[len(gc.parts)-1]
		lastText := isText(last["text"])
		_, sig1 := p["thoughtSignature"]
		_, sig2 := p["thought_signature"]
		if lastText && !sig1 && !sig2 && boolOf(last["thought"]) == boolOf(p["thought"]) {
			appendStr(last, "text", t)
			return
		}
	}
	cp := obj{}
	for k, v := range p {
		cp[k] = v
	}
	gc.parts = append(gc.parts, cp)
}

func boolOf(v any) bool { b, _ := v.(bool); return b }

func (g *gemini) finish(status string, at time.Time, terminal bool) Response {
	idx := make([]int, 0, len(g.cands))
	for i := range g.cands {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	cands := make([]any, 0, len(idx))
	anyFinish := false
	for _, i := range idx {
		gc := g.cands[i]
		parts := make([]any, len(gc.parts))
		for k, p := range gc.parts {
			parts[k] = p
		}
		content := obj{"parts": parts}
		if gc.role != nil {
			content["role"] = gc.role
		}
		gc.c["content"] = content
		if len(gc.chunks) > 0 {
			if gm, ok := gc.c["groundingMetadata"].(map[string]any); ok {
				gm["groundingChunks"] = gc.chunks
			}
		}
		if _, ok := gc.c["index"]; !ok {
			gc.c["index"] = json.Number(itoa(i))
		}
		if gc.finish {
			anyFinish = true
		}
		cands = append(cands, gc.c)
	}
	if status == StatusCompleted && !anyFinish && !terminal {
		status = StatusIncomplete
	}
	g.top["candidates"] = cands
	r := Response{ResponseID: str(g.top, "responseId"), Body: marshal(g.top), TerminalStatus: status}
	if terminal {
		r.TerminalAt = at
	}
	g.open = false
	return r
}

func (g *gemini) Flush(status string, at time.Time) []Response {
	if !g.open {
		return nil
	}
	return []Response{g.finish(status, at, false)}
}

// ---- generic (interactions, openai-image, unknown) ----

type generic struct {
	events       []any
	open         bool
	id           string
	interactions bool
}

// stubThoughtSignatures replaces the signature of every Interactions thought_signature stream
// delta ({"type":"thought_signature","signature":…}) with the canon/v1 opaque stub, hashing the
// string as sent. canon stubs the signature of thought steps, but a stream
// carries it in these deltas, which the generic split keeps as events. Tool payloads
// (arguments, result) are user data and are left alone.
func stubThoughtSignatures(v any) {
	switch x := v.(type) {
	case map[string]any:
		if str(x, "type") == "thought_signature" {
			if s, ok := x["signature"].(string); ok {
				sum := sha256.Sum256([]byte(s))
				x["signature"] = obj{"$opaque": obj{"sha256": hex.EncodeToString(sum[:]), "len": json.Number(itoa(len(s)))}}
			}
		}
		for k, e := range x {
			if k != "arguments" && k != "result" {
				stubThoughtSignatures(e)
			}
		}
	case []any:
		for _, e := range x {
			stubThoughtSignatures(e)
		}
	}
}

func (g *generic) Feed(ev Event, at time.Time) []Response {
	if bytes.Equal(ev.Data, done) {
		return nil
	}
	o, ok := decode(ev.Data)
	if !ok {
		return nil
	}
	g.open = true
	if t := typeOf(ev, o); t != "" {
		if _, has := o["type"]; !has {
			o["type"] = t
		}
	}
	if s := str(o, "partial_image_b64"); s != "" {
		sum, n := hashBase64(s)
		delete(o, "partial_image_b64")
		o["partial_image"] = obj{"sha256": sum, "len": json.Number(itoa(n))}
	}
	if id := str(o, "id"); id != "" && g.id == "" {
		g.id = id
	}
	if g.interactions {
		stubThoughtSignatures(o)
	}
	g.events = append(g.events, o)
	return nil
}

func (g *generic) Flush(status string, at time.Time) []Response {
	if !g.open {
		return nil
	}
	g.open = false
	body := marshal(obj{"object": "stream.events", "events": g.events})
	g.events = nil
	return []Response{{ResponseID: g.id, Body: body, TerminalStatus: status}}
}
