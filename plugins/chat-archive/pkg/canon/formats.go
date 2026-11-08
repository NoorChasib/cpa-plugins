package canon

import (
	"errors"
	"net/url"
	"strings"
)

// Block payload rules, one section per format. Every block function starts a unit, takes the volatile
// fields out, stubs attachments in the whole block and opaque fields only at their named positions,
// moves the payload to P, and lets finishBlock mask, canonicalize and hash. Opaque stubbing never descends into tool payloads (tool_use input, tool
// results, function arguments and responses): an object there shaped like a thinking block or a part
// with a thoughtSignature is user data, masked like every other string, not hashed (a bare sha256 and
// length of a secret would confirm a guess).

// textBlock synthesizes the text block a string content is normalized to.
func textBlock(typ string, text *node) *node {
	b := objNode()
	b.set("type", strNode(typ))
	b.set("text", text)
	return b
}

// typeFrame returns {"type": <b's type value>} ({} without one) for the "any other block" rules.
func typeFrame(b *node) *node {
	f := objNode()
	if t := b.get("type"); t != nil {
		f.set("type", t)
	}
	return f
}

// contentBlocks takes a message's content: a string becomes one text block of type typ and the frame
// records "content_form":"string"; an array is its blocks; anything else stays in the frame.
func contentBlocks(m *node, typ string) []*node {
	c := m.get("content")
	switch {
	case c.isString():
		m.del("content")
		m.set("content_form", strNode("string"))
		return []*node{textBlock(typ, c)}
	case c.isArray():
		m.del("content")
		return c.elems
	}
	return nil
}

// ---- Claude Messages ----

func (s *splitter) claudeRequest(body *node) {
	switch sys := body.get("system"); {
	case sys.isString():
		body.del("system")
		s.claudeBlock(&s.sys, textBlock("text", sys))
	case sys.isArray():
		body.del("system")
		for _, b := range sys.elems {
			s.claudeBlock(&s.sys, b)
		}
	}
	if msgs := body.get("messages"); msgs.isArray() {
		body.del("messages")
		for _, m := range msgs.elems {
			s.claudeMessage(&s.items, m)
		}
	}
	s.toolsFrom(body, "tools")
	s.startUnit()
	s.skeletonPiece(body)
}

// claudeResponse: the response is one assistant message; Skeleton is the body minus content.
func (s *splitter) claudeResponse(body *node) {
	if c := body.get("content"); c != nil {
		msg := objNode()
		if r := body.get("role"); r != nil {
			msg.set("role", r)
		}
		msg.set("content", body.del("content"))
		s.claudeMessage(&s.items, msg)
	}
	s.startUnit()
	s.skeletonPiece(body)
}

func (s *splitter) claudeMessage(l *itemList, m *node) {
	s.startUnit()
	if !m.isObject() {
		s.msgItem(l, m)
		return
	}
	blocks := contentBlocks(m, "text")
	s.msgItem(l, m)
	for _, b := range blocks {
		s.claudeBlock(l, b)
	}
}

func (s *splitter) claudeBlock(l *itemList, b *node) {
	s.startUnit()
	if !b.isObject() {
		s.frameOnly(l, b)
		return
	}
	v := takeVolatile(b)
	walk(b, s.claudeVisit)
	s.claudeOpaque(b)
	frame := b
	var text *string
	var js *node
	switch b.strField("type") {
	case "text":
		text = b.takeString("text")
	case "thinking":
		text = b.takeString("thinking")
	case "redacted_thinking", "image":
	case "tool_use", "server_tool_use", "mcp_tool_use":
		js = b.del("input")
	case "tool_result", "mcp_tool_result":
		switch c := b.get("content"); {
		case c.isString():
			text = b.takeString("content")
		case c.isArray():
			js = b.del("content")
		}
	case "document":
		if src := b.get("source"); src.isObject() && src.strField("type") == "text" {
			text = src.takeString("data")
		}
	case "compaction":
		text = b.takeString("content")
	default:
		js = b.without("type")
		frame = typeFrame(b)
	}
	s.finishBlock(l, frame, text, js, v)
}

// claudeVisit stubs base64 image/document sources, also nested in tool_result content and other
// blocks.
func (s *splitter) claudeVisit(o *node) {
	switch o.strField("type") {
	case "image", "document":
		if src := o.get("source"); src.isObject() && src.strField("type") == "base64" {
			mime := src.strField("media_type")
			stubField(src, "data", func(x string) (*node, bool) { return s.b64Stub(x, mime) })
		}
	}
}

// claudeOpaque stubs the Claude opaque fields of block b itself, and the advisor_redacted_result a
// server tool result (advisor_tool_result) carries as its content.
func (s *splitter) claudeOpaque(b *node) {
	typ := b.strField("type")
	switch typ {
	case "thinking", "signature_delta":
		s.opaqueField(b, "signature")
	case "redacted_thinking":
		s.opaqueField(b, "data")
	case "compaction":
		s.opaqueField(b, "encrypted_content")
		s.opaqueField(b, "signature")
	case "advisor_redacted_result":
		s.opaqueField(b, "encrypted_content")
	case "tool_result", "mcp_tool_result":
	default:
		if !strings.HasSuffix(typ, "_tool_result") {
			return
		}
		c := b.get("content")
		results := []*node{c}
		if c.isArray() {
			results = c.elems
		}
		for _, r := range results {
			if r.isObject() && r.strField("type") == "advisor_redacted_result" {
				s.opaqueField(r, "encrypted_content")
			}
		}
	}
}

// ---- OpenAI chat completions ----

func (s *splitter) chatRequest(body *node) {
	if msgs := body.get("messages"); msgs.isArray() {
		body.del("messages")
		for _, m := range msgs.elems {
			s.chatMessage(&s.items, m)
		}
	}
	s.toolsFrom(body, "tools")
	s.startUnit()
	s.skeletonPiece(body)
}

// chatResponse: choices[0].message is the response message; other choices stay in Skeleton as
// alternates.
func (s *splitter) chatResponse(body *node) {
	if ch := body.get("choices"); ch.isArray() && len(ch.elems) > 0 && ch.elems[0].isObject() {
		if m := ch.elems[0].del("message"); m != nil {
			s.chatMessage(&s.items, m)
		}
	}
	s.startUnit()
	s.skeletonPiece(body)
}

// chatMessage emits the message frame, then reasoning_content, reasoning, reasoning_details, the
// content parts and the tool calls, one item each.
func (s *splitter) chatMessage(l *itemList, m *node) {
	s.startUnit()
	if !m.isObject() {
		s.msgItem(l, m)
		return
	}
	rc := m.takeString("reasoning_content")
	rs := m.takeString("reasoning")
	rd := m.get("reasoning_details")
	if rd != nil && rd.kind != kNull {
		m.del("reasoning_details")
	} else {
		rd = nil
	}
	parts := contentBlocks(m, "text")
	var calls []*node
	if tc := m.get("tool_calls"); tc.isArray() {
		m.del("tool_calls")
		calls = tc.elems
	}
	if imgs := m.get("images"); imgs.isArray() { // CPA translators' generated images (data URLs)
		for _, el := range imgs.elems {
			walk(el, s.chatVisit)
		}
	}
	s.msgItem(l, m)
	if rc != nil {
		s.startUnit()
		s.finishBlock(l, typeNode("reasoning_content"), rc, nil, nil)
	}
	if rs != nil {
		s.startUnit()
		s.finishBlock(l, typeNode("reasoning"), rs, nil, nil)
	}
	if rd != nil {
		s.startUnit()
		entries := []*node{rd}
		if rd.isArray() {
			entries = rd.elems
		}
		for _, e := range entries {
			if e.isObject() {
				if e.strField("type") == "reasoning.encrypted" {
					s.opaqueField(e, "data")
				}
				s.opaqueField(e, "signature")
			}
		}
		s.finishBlock(l, typeNode("reasoning_details"), nil, rd, nil)
	}
	for _, p := range parts {
		s.chatPart(l, p)
	}
	for _, tc := range calls {
		s.chatToolCall(l, tc)
	}
}

func typeNode(t string) *node {
	f := objNode()
	f.set("type", strNode(t))
	return f
}

func (s *splitter) chatPart(l *itemList, p *node) {
	s.startUnit()
	if !p.isObject() {
		s.frameOnly(l, p)
		return
	}
	v := takeVolatile(p)
	walk(p, s.chatVisit)
	var text *string
	if p.strField("type") == "text" {
		text = p.takeString("text")
	}
	s.finishBlock(l, p, text, nil, v)
}

// chatVisit stubs image_url.url data URLs, file.file_data (data URL or bare base64) and
// input_audio.data (bare base64).
func (s *splitter) chatVisit(o *node) {
	if iu := o.get("image_url"); iu.isObject() {
		stubField(iu, "url", s.dataURLStub)
	}
	if f := o.get("file"); f.isObject() {
		stubField(f, "file_data", func(x string) (*node, bool) { return s.dataOrB64Stub(x, "") })
	}
	if a := o.get("input_audio"); a.isObject() {
		mime := ""
		if f := a.strField("format"); f != "" {
			mime = "audio/" + f
		}
		stubField(a, "data", func(x string) (*node, bool) { return s.b64Stub(x, mime) })
	}
}

func (s *splitter) chatToolCall(l *itemList, tc *node) {
	s.startUnit()
	if !tc.isObject() {
		s.frameOnly(l, tc)
		return
	}
	v := takeVolatile(tc)
	if ec := tc.get("extra_content"); ec.isObject() {
		if g := ec.get("google"); g.isObject() {
			s.opaqueField(g, "thought_signature")
		}
	}
	var text *string
	var js *node
	if fn := tc.get("function"); fn.isObject() {
		if a := fn.takeString("arguments"); a != nil {
			text, js = s.arguments(*a)
		}
	}
	s.finishBlock(l, tc, text, js, v)
}

// arguments canonicalizes a JSON-string tool argument: a string that
// parses becomes the canonical (masked) serialization of its value, still a JSON string, as a json
// piece; anything else is a text piece with flag args_raw.
func (s *splitter) arguments(x string) (*string, *node) {
	n, p, err := s.parse([]byte(x))
	if errors.Is(err, ErrBodyBudget) {
		s.err = err
		return &x, nil
	}
	if err != nil {
		s.flag("args_raw")
		return &x, nil
	}
	s.parseFlags(p)
	return nil, strNode(string(s.enc(n)))
}

// ---- OpenAI Responses (HTTP bodies and WebSocket response.create/append/steer messages) ----

// responsesTop splits a Responses request (items under "input") or response object (under "output"):
// instructions is one text system item, tools the Tools piece, and the rest of the body the Skeleton.
// WebSocket client messages carry the same fields plus type, previous_response_id, event_id, stream_id
// and generate, which stay in the Skeleton.
func (s *splitter) responsesTop(body *node, itemsKey string) {
	if ins := body.get("instructions"); ins != nil && ins.kind != kNull {
		body.del("instructions")
		s.startUnit()
		var text *string
		var js *node
		if ins.isString() {
			text = &ins.s
		} else {
			js = ins
		}
		s.finishBlock(&s.sys, typeNode("instructions"), text, js, nil)
	}
	switch in := body.get(itemsKey); {
	case in.isString():
		body.del(itemsKey)
		m := objNode()
		m.set("content", in)
		s.responsesMessage(&s.items, m)
	case in.isArray():
		body.del(itemsKey)
		for _, it := range in.elems {
			s.responsesItem(&s.items, it)
		}
	}
	s.toolsFrom(body, "tools")
	s.startUnit()
	s.skeletonPiece(body)
}

func (s *splitter) responsesItem(l *itemList, it *node) {
	s.startUnit()
	if !it.isObject() {
		s.frameOnly(l, it)
		return
	}
	typ := it.strField("type")
	if typ == "message" || (it.get("type") == nil && it.has("role")) {
		s.responsesMessage(l, it)
		return
	}
	v := takeVolatile(it)
	walk(it, s.responsesVisit)
	if typ == "reasoning" || typ == "compaction" {
		s.opaqueField(it, "encrypted_content")
	}
	frame := it
	var text *string
	var js *node
	switch typ {
	case "function_call":
		if a := it.takeString("arguments"); a != nil {
			text, js = s.arguments(*a)
		}
	case "custom_tool_call":
		text = it.takeString("input")
	case "function_call_output", "custom_tool_call_output":
		switch o := it.get("output"); {
		case o.isString():
			text = it.takeString("output")
		case o.isArray():
			js = it.del("output")
		}
	case "reasoning":
		p := objNode()
		for _, k := range [...]string{"summary", "content"} {
			if x := it.del(k); x != nil {
				p.set(k, x)
			}
		}
		if len(p.members) > 0 {
			js = p
		}
	case "compaction":
	default:
		js = it.without("type")
		frame = typeFrame(it)
	}
	s.finishBlock(l, frame, text, js, v)
}

func (s *splitter) responsesMessage(l *itemList, m *node) {
	s.startUnit()
	parts := contentBlocks(m, "input_text")
	s.msgItem(l, m)
	for _, p := range parts {
		s.responsesPart(l, p)
	}
}

func (s *splitter) responsesPart(l *itemList, p *node) {
	s.startUnit()
	if !p.isObject() {
		s.frameOnly(l, p)
		return
	}
	v := takeVolatile(p)
	walk(p, s.responsesVisit)
	frame := p
	var text *string
	var js *node
	switch p.strField("type") {
	case "input_text", "output_text":
		text = p.takeString("text")
	case "input_image", "input_file":
	default:
		js = p.without("type")
		frame = typeFrame(p)
	}
	s.finishBlock(l, frame, text, js, v)
}

// responsesVisit stubs image_url data URLs (input_image, computer screenshots, nested output parts),
// file_data and image_generation_call.result. The opaque encrypted_content of reasoning and compaction
// items is stubbed on the item itself (responsesItem).
func (s *splitter) responsesVisit(o *node) {
	stubField(o, "image_url", s.dataURLStub)
	stubField(o, "file_data", func(x string) (*node, bool) { return s.dataOrB64Stub(x, "") })
	if o.strField("type") == "image_generation_call" {
		stubField(o, "result", func(x string) (*node, bool) { return s.b64Stub(x, "") })
	}
}

// ---- Gemini generateContent ----

var geminiDataFields = [...]string{"functionCall", "function_call", "functionResponse", "function_response",
	"executableCode", "executable_code", "codeExecutionResult", "code_execution_result"}

func (s *splitter) geminiRequest(body *node) {
	for _, k := range [...]string{"systemInstruction", "system_instruction"} {
		if si := body.get(k); si.isObject() {
			if parts := si.get("parts"); parts.isArray() {
				si.del("parts")
				for _, p := range parts.elems {
					s.geminiPart(&s.sys, p)
				}
				if len(si.members) == 0 {
					body.del(k)
				}
			}
		}
	}
	if cs := body.get("contents"); cs.isArray() {
		body.del("contents")
		for _, c := range cs.elems {
			s.geminiContent(&s.items, c)
		}
	}
	s.toolsFrom(body, "tools")
	s.startUnit()
	s.skeletonPiece(body)
}

// geminiResponse: candidates[0].content is the response; other candidates stay in Skeleton.
func (s *splitter) geminiResponse(body *node) {
	if cs := body.get("candidates"); cs.isArray() && len(cs.elems) > 0 && cs.elems[0].isObject() {
		if c := cs.elems[0].del("content"); c != nil {
			s.geminiContent(&s.items, c)
		}
	}
	s.startUnit()
	s.skeletonPiece(body)
}

func (s *splitter) geminiContent(l *itemList, c *node) {
	s.startUnit()
	if !c.isObject() {
		s.msgItem(l, c)
		return
	}
	var parts []*node
	if p := c.get("parts"); p.isArray() {
		c.del("parts")
		parts = p.elems
	}
	s.msgItem(l, c)
	for _, p := range parts {
		s.geminiPart(l, p)
	}
}

// geminiPart: text is the payload (the frame keeps thought and the stubbed signature); a function or
// code part's object is a json payload and the frame records the field name as "type"; inlineData is
// stubbed in place; any other part is kept whole as its frame.
func (s *splitter) geminiPart(l *itemList, p *node) {
	s.startUnit()
	if !p.isObject() {
		s.frameOnly(l, p)
		return
	}
	v := takeVolatile(p)
	walk(p, s.geminiVisit)
	s.opaqueField(p, "thoughtSignature")
	s.opaqueField(p, "thought_signature")
	var text *string
	var js *node
	if text = p.takeString("text"); text == nil {
		for _, k := range geminiDataFields {
			if d := p.del(k); d != nil {
				js = d
				p.set("type", strNode(k))
				break
			}
		}
	}
	s.finishBlock(l, p, text, js, v)
}

func (s *splitter) geminiVisit(o *node) {
	for _, k := range [...]string{"inlineData", "inline_data"} {
		if d := o.get(k); d.isObject() {
			mime := d.strField("mimeType")
			if mime == "" {
				mime = d.strField("mime_type")
			}
			stubField(d, "data", func(x string) (*node, bool) { return s.b64Stub(x, mime) })
		}
	}
}

// ---- Gemini Interactions (generic split) ----

func (s *splitter) interactionsRequest(body *node) {
	if si := body.get("system_instruction"); si != nil && si.kind != kNull {
		body.del("system_instruction")
		s.startUnit()
		s.interactionsWalk(si)
		var text *string
		var js *node
		if si.isString() {
			text = &si.s
		} else {
			js = si
		}
		s.finishBlock(&s.sys, typeNode("system_instruction"), text, js, nil)
	}
	switch in := body.get("input"); {
	case in.isString():
		body.del("input")
		s.startUnit()
		s.finishBlock(&s.items, typeNode("text"), &in.s, nil, nil)
	case in.isArray():
		body.del("input")
		for _, el := range in.elems {
			s.interactionsElem(&s.items, el)
		}
	case in.isObject():
		body.del("input")
		s.interactionsElem(&s.items, in)
	}
	s.toolsFrom(body, "tools")
	s.startUnit()
	s.skeletonPiece(body)
}

// interactionsElem: one json piece per input element; the frame keeps its type.
func (s *splitter) interactionsElem(l *itemList, el *node) {
	s.startUnit()
	if !el.isObject() {
		s.frameOnly(l, el)
		return
	}
	v := takeVolatile(el)
	s.interactionsWalk(el)
	s.finishBlock(l, typeFrame(el), nil, el.without("type"), v)
}

// interactionsResponse: the final interaction is one json piece with thought signatures stubbed.
func (s *splitter) interactionsResponse(body *node) {
	s.startUnit()
	s.interactionsWalk(body)
	s.finishBlock(&s.items, typeNode("interaction"), nil, body, nil)
}

// interactionsWalk stubs, in n's subtree, the base64 data of content objects that carry a mime_type
// (image, audio, video, document) and the signature of thought steps. Thought signatures are not
// looked for inside function call arguments and function results (tool payloads).
func (s *splitter) interactionsWalk(n *node) {
	switch n.kind {
	case kObject:
		if n.strField("type") == "thought" {
			s.opaqueField(n, "signature")
		}
		mime := n.strField("mime_type")
		if mime == "" {
			mime = n.strField("mimeType")
		}
		if mime != "" {
			stubField(n, "data", func(x string) (*node, bool) { return s.b64Stub(x, mime) })
		}
		for _, m := range n.members {
			if m.key == "arguments" || m.key == "result" {
				walk(m.val, s.interactionsAttVisit)
				continue
			}
			s.interactionsWalk(m.val)
		}
	case kArray:
		for _, el := range n.elems {
			s.interactionsWalk(el)
		}
	}
}

// interactionsAttVisit stubs base64 data with a mime_type inside a tool payload (attachments only).
func (s *splitter) interactionsAttVisit(o *node) {
	mime := o.strField("mime_type")
	if mime == "" {
		mime = o.strField("mimeType")
	}
	if mime != "" {
		stubField(o, "data", func(x string) (*node, bool) { return s.b64Stub(x, mime) })
	}
}

// ---- media.* (images, videos, speech JSON) ----

// media stores the whole body as the Skeleton with every base64 data URL and b64_json value as an
// attachment. Responses first have every absolute http(s) URL replaced by StripURL.
func (s *splitter) media(body *node, response bool) {
	if response {
		s.mediaURLs = stripURLs(body, s.stripURL, s.mediaURLs)
	}
	s.startUnit()
	s.mediaWalk(body)
	s.skeletonPiece(body)
}

func (s *splitter) mediaWalk(n *node) {
	switch n.kind {
	case kObject:
		mime := n.strField("mime_type")
		for i, m := range n.members {
			if m.val.isString() {
				var st *node
				var ok bool
				if m.key == "b64_json" {
					st, ok = s.b64Stub(m.val.s, mime)
				} else {
					st, ok = s.dataURLStub(m.val.s)
				}
				if ok {
					n.members[i].val = st
				}
				continue
			}
			s.mediaWalk(m.val)
		}
	case kArray:
		for i, el := range n.elems {
			if el.isString() {
				if st, ok := s.dataURLStub(el.s); ok {
					n.elems[i] = st
				}
				continue
			}
			s.mediaWalk(el)
		}
	}
}

// stripURLs replaces, in document order, every string value that is an absolute http/https URL by
// strip(url) and appends the URLs found to urls.
func stripURLs(n *node, strip func(string) string, urls []MediaURL) []MediaURL {
	switch n.kind {
	case kString:
		if isHTTPURL(n.s) {
			masked := strip(n.s)
			urls = append(urls, MediaURL{URL: n.s, MaskedURL: masked})
			n.s = masked
		}
	case kObject:
		for _, m := range n.members {
			urls = stripURLs(m.val, strip, urls)
		}
	case kArray:
		for _, el := range n.elems {
			urls = stripURLs(el, strip, urls)
		}
	}
	return urls
}

func isHTTPURL(x string) bool {
	if len(x) < 8 || !(strings.EqualFold(x[:7], "http://") || strings.EqualFold(x[:8], "https://")) {
		return false
	}
	u, err := url.Parse(x)
	return err == nil && u.Host != "" && (strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https"))
}
