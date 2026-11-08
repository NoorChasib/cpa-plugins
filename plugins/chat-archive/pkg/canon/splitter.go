package canon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// memoMin is the shortest string the Options.Memo is consulted for. The memo exists to skip
// re-masking large unchanged blocks; short keys and values are masked directly.
const memoMin = 1 << 10

// splitRefs holds, per unit of a Split, the piece and attachment hashes it references, so Refs can
// leave out the units a prefix-delta base already carries.
type splitRefs struct {
	skel, tools []wire.Hash
	sys, items  [][]wire.Hash
}

// itemList collects items with their per-item reference lists.
type itemList struct {
	items []wire.Item
	refs  [][]wire.Hash
}

func (l *itemList) add(it wire.Item, refs []wire.Hash) {
	l.items = append(l.items, it)
	l.refs = append(l.refs, refs)
}

// splitter holds the state of one split: the masker, the pieces found so far and the result parts.
type splitter struct {
	ctx      context.Context
	m        stringMasker
	stripURL func(string) string
	memo     Memo
	maxAtt   int64
	maxDec   int64 // Options.MaxDecoded, or MaxGunzip
	nodes    int   // JSON values the split may still parse; < 0 means no cap (Options.MaxNodes)
	err      error // sticky: ctx ended during masking, or the node budget ran out

	flags    map[string]bool
	hits     mask.Hits
	pieces   map[wire.Hash]Piece
	oversize int
	cur      []wire.Hash // references of the unit being built

	skel, tools         *wire.Hash
	skelRefs, toolsRefs []wire.Hash
	toolsV              []byte
	sys, items          itemList
	mediaURLs           []MediaURL
}

func newSplitter(ctx context.Context, m stringMasker, stripURL func(string) string, opts Options) *splitter {
	maxAtt := opts.MaxAttachment
	if maxAtt <= 0 {
		maxAtt = wire.DefaultLimits.Attachment
	}
	maxDec := opts.MaxDecoded
	if maxDec <= 0 {
		maxDec = MaxGunzip
	}
	nodes := opts.MaxNodes
	if nodes <= 0 {
		nodes = -1
	}
	return &splitter{ctx: ctx, m: m, stripURL: stripURL, memo: opts.Memo, maxAtt: maxAtt, maxDec: maxDec,
		nodes: nodes, pieces: make(map[wire.Hash]Piece)}
}

// parse parses b against the split's node budget (Options.MaxNodes).
func (s *splitter) parse(b []byte) (*node, parser, error) {
	n, p, err := parseBudget(b, s.nodes)
	if s.nodes >= 0 {
		s.nodes = max(p.left, 0)
	}
	return n, p, err
}

func (s *splitter) flag(f string) {
	if s.flags == nil {
		s.flags = make(map[string]bool)
	}
	s.flags[f] = true
}

func (s *splitter) parseFlags(p parser) {
	if p.dup {
		s.flag(wire.FlagDupKeys)
	}
	if p.bad {
		s.flag(wire.FlagBadUTF8)
	}
}

// maskStr masks one decoded string. After ctx ends it records ctx.Err() and returns
// "" so nothing unmasked reaches the output; the split then fails.
func (s *splitter) maskStr(x string) string {
	out, hits, ok := s.mask(x)
	if !ok {
		return ""
	}
	s.hits = addHits(s.hits, hits)
	return out
}

// mask masks x without counting hits: the memo, then the masker. With a *mask.Masker it uses
// StringContext, so the deadline stops one long string too. ok is false once the split
// has failed.
func (s *splitter) mask(x string) (string, mask.Hits, bool) {
	if s.err != nil {
		return "", nil, false
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return "", nil, false
	}
	var key wire.Hash
	memo := s.memo != nil && len(x) >= memoMin
	if memo {
		h := sha256.New()
		io.WriteString(h, x)
		h.Sum(key[:0])
		if out, hits, ok := s.memo.Get(key); ok {
			return out, hits, true
		}
	}
	var out string
	var hits mask.Hits
	if cm, ok := s.m.(ctxMasker); ok {
		var err error
		if out, hits, err = cm.StringContext(s.ctx, x); err != nil {
			s.err = err
			return "", nil, false
		}
	} else {
		out, hits = s.m.String(x)
	}
	if memo {
		s.memo.Add(key, out, hits)
	}
	return out, hits, true
}

// enc returns the masked canon/v1 bytes of n.
func (s *splitter) enc(n *node) []byte {
	e := encoder{mask: s.maskStr}
	e.value(n)
	if e.dup {
		s.flag(wire.FlagDupKeys)
	}
	return e.buf
}

func (s *splitter) addPiece(kind wire.PieceKind, contentType string, b []byte) wire.Hash {
	p := NewPiece(kind, contentType, b)
	if _, ok := s.pieces[p.Hash]; !ok {
		s.pieces[p.Hash] = p
	}
	return p.Hash
}

func (s *splitter) jsonPiece(n *node) wire.Hash { return s.addPiece(wire.PieceJSON, "", s.enc(n)) }

func (s *splitter) textPiece(x string) wire.Hash {
	return s.addPiece(wire.PieceText, "", []byte(s.maskStr(x)))
}

func (s *splitter) startUnit() { s.cur = nil }

func (s *splitter) endUnit() []wire.Hash {
	r := s.cur
	s.cur = nil
	return r
}

// finishBlock appends one block item: frame F, at most one payload (text or json) as P, and the
// volatile fields as V.
func (s *splitter) finishBlock(l *itemList, frame *node, text *string, js *node, v *node) {
	it := wire.Item{F: s.enc(frame)}
	var p wire.Hash
	switch {
	case text != nil:
		p = s.textPiece(*text)
	case js != nil:
		p = s.jsonPiece(js)
	}
	if text != nil || js != nil {
		it.P = &p
		s.cur = append(s.cur, p)
	}
	if v != nil {
		it.V = s.enc(v)
	}
	l.add(it, s.endUnit())
}

// frameOnly appends a block item that is its frame alone (non-object blocks, frame-only rules).
func (s *splitter) frameOnly(l *itemList, n *node) {
	l.add(wire.Item{F: s.enc(n)}, s.endUnit())
}

// msgItem appends one message frame item.
func (s *splitter) msgItem(l *itemList, m *node) {
	l.add(wire.Item{M: s.enc(m)}, s.endUnit())
}

// skeletonPiece stores n as the Skeleton json piece; the references of the current unit (attachments
// found in n) stay with it.
func (s *splitter) skeletonPiece(n *node) {
	h := s.jsonPiece(n)
	s.skel = &h
	s.skelRefs = append(s.endUnit(), h)
}

// toolsFrom moves an array body[key] to the Tools piece, with per-tool cache_control in ToolsV.
func (s *splitter) toolsFrom(body *node, key string) {
	t := body.get(key)
	if !t.isArray() {
		return
	}
	body.del(key)
	var tv []*node
	for i, el := range t.elems {
		if el.isObject() {
			if cc := el.del("cache_control"); cc != nil {
				v := objNode()
				v.set("cache_control", cc)
				e := objNode()
				e.set("i", numNode(i))
				e.set("v", v)
				tv = append(tv, e)
			}
		}
	}
	s.startUnit()
	h := s.jsonPiece(t)
	s.tools = &h
	s.toolsRefs = append(s.endUnit(), h)
	if tv != nil {
		s.toolsV = s.enc(arrNode(tv))
	}
}

// volatileKeys are removed from every block, part and item to V before hashing.
var volatileKeys = [...]string{"cache_control", "prompt_cache_breakpoint"}

func takeVolatile(n *node) *node {
	var v *node
	for _, k := range volatileKeys {
		if val := n.del(k); val != nil {
			if v == nil {
				v = objNode()
			}
			v.set(k, val)
		}
	}
	return v
}

// walk calls visit on every object in n's subtree, parents before children. Stubs are not entered.
func walk(n *node, visit func(*node)) {
	switch n.kind {
	case kObject:
		visit(n)
		for _, m := range n.members {
			walk(m.val, visit)
		}
	case kArray:
		for _, el := range n.elems {
			walk(el, visit)
		}
	}
}

// opaqueField replaces o[k] by an $opaque stub when it is a string.
func (s *splitter) opaqueField(o *node, k string) {
	if v := o.get(k); v.isString() {
		o.set(k, opaqueStub(v.s))
	}
}

func opaqueStub(x string) *node {
	h := sha256.Sum256([]byte(x))
	b := append([]byte(`{"$opaque":{"len":`), strconv.Itoa(len(x))...)
	b = append(b, `,"sha256":"`...)
	b = hex.AppendEncode(b, h[:])
	return rawNode(append(b, `"}}`...))
}

// attach turns decoded bytes into an attachment and returns its $att stub.
// Bytes that are valid UTF-8 are masked as one string whatever the declared type (PNG, JPEG, zip and
// compressed PDF bytes are practically never valid UTF-8, so binary media are unaffected). A declared
// text type whose bytes are not valid UTF-8 (a Latin-1 file) has each maximal valid UTF-8 run masked,
// the invalid bytes kept between them. Either way the hash is over the masked bytes and the stub says
// "masked":true. Anything else is binary, hashed raw (a secret inside binary bytes stays unmasked). An attachment over the limit
// keeps its stub with "dropped":"oversize" and gets no piece.
func (s *splitter) attach(data []byte, mime, enc string) *node {
	mime = s.mediaType(mime, data)
	masked := false
	switch {
	case utf8.Valid(data):
		data = []byte(s.maskStr(string(data)))
		masked = true
	case isTextMedia(mime):
		data = s.maskRuns(data)
		masked = true
	}
	h := sha256.Sum256(data)
	dropped := int64(len(data)) > s.maxAtt
	if dropped {
		s.oversize++
	} else {
		s.addPiece(wire.PieceAttachment, mime, data)
		s.cur = append(s.cur, h)
	}
	b := []byte(`{"$att":{`)
	if dropped {
		b = append(b, `"dropped":"`+wire.DroppedOversize+`",`...)
	}
	b = append(b, `"enc":`...)
	b = appendString(b, enc)
	b = append(b, `,"len":`...)
	b = strconv.AppendInt(b, int64(len(data)), 10)
	if masked {
		b = append(b, `,"masked":true`...)
	}
	b = append(b, `,"mime":`...)
	b = appendString(b, mime)
	b = append(b, `,"sha256":"`...)
	b = hex.AppendEncode(b, h[:])
	return rawNode(append(b, `"}}`...))
}

// maskRuns masks each maximal valid UTF-8 run of data as one string and keeps the invalid bytes
// between runs.
func (s *splitter) maskRuns(data []byte) []byte {
	out := make([]byte, 0, len(data))
	utf8Runs(data, func(seg []byte, valid bool) {
		if valid {
			out = append(out, s.maskStr(string(seg))...)
		} else {
			out = append(out, seg...)
		}
	})
	return out
}

// utf8Runs calls fn, in order, with each maximal valid UTF-8 run of b (valid) and each byte that
// starts no valid UTF-8 sequence (not valid).
func utf8Runs(b []byte, fn func(seg []byte, valid bool)) {
	start := 0
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			if start < i {
				fn(b[start:i], true)
			}
			fn(b[i:i+1], false)
			i++
			start = i
			continue
		}
		i += size
	}
	if start < len(b) {
		fn(b[start:], true)
	}
}

// attachable reports whether decoded payload bytes become an attachment: always at MinAttachment
// bytes or more; below that only when they are text (valid UTF-8, or the valid runs of a declared
// text type) that masking changes, so a small secret-bearing document is never left base64-encoded in
// a frame, and small payloads without secrets stay inline as before.
func (s *splitter) attachable(b []byte, declared string) bool {
	if len(b) >= MinAttachment {
		return true
	}
	if len(b) == 0 {
		return false
	}
	if utf8.Valid(b) {
		out, _, ok := s.mask(string(b))
		return ok && out != string(b)
	}
	if !isTextMedia(normMediaType(declared)) {
		return false
	}
	changed := false
	utf8Runs(b, func(seg []byte, valid bool) {
		if valid && !changed {
			out, _, ok := s.mask(string(seg))
			changed = ok && out != string(seg)
		}
	})
	return changed
}

// mediaType normalizes a declared media type to lowercase type/subtype without parameters. A missing
// or malformed one is sniffed from the bytes (http.DetectContentType), so text without a declared type
// is still masked. A type the masker would change is replaced, so stubs hold no secret.
func (s *splitter) mediaType(declared string, data []byte) string {
	m := normMediaType(declared)
	if !validMediaType(m) {
		m = http.DetectContentType(data)
		if i := strings.IndexByte(m, ';'); i >= 0 {
			m = strings.TrimSpace(m[:i])
		}
	}
	if s.maskStr(m) != m {
		return "application/octet-stream"
	}
	return m
}

// normMediaType lowercases a declared media type and drops its parameters.
func normMediaType(declared string) string {
	m := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// validMediaType accepts type/subtype made of RFC 6838 restricted-name characters, at most 127 bytes.
func validMediaType(m string) bool {
	if len(m) == 0 || len(m) > 127 {
		return false
	}
	slash := strings.IndexByte(m, '/')
	if slash <= 0 || slash == len(m)-1 || strings.Count(m, "/") != 1 {
		return false
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '/':
		case strings.IndexByte("!#$&^_.+-", c) >= 0 && i != 0 && i != slash+1:
		default:
			return false
		}
	}
	return true
}

// isTextMedia lists the declared text media types: text/*, *+json, *+xml,
// application/json, application/x-yaml and application/xml. A valid UTF-8 attachment is masked
// whatever its type; this list decides only whether invalid UTF-8 bytes are masked run by run.
func isTextMedia(m string) bool {
	switch m {
	case "application/json", "application/x-yaml", "application/xml":
		return true
	}
	return strings.HasPrefix(m, "text/") || strings.HasSuffix(m, "+json") || strings.HasSuffix(m, "+xml")
}

// b64Encodings are the four RFC 4648 base64 forms decodeB64 accepts.
var b64Encodings = [...]struct {
	enc       *base64.Encoding
	url, pads bool
}{
	{base64.StdEncoding, false, true},
	{base64.RawStdEncoding, false, false},
	{base64.URLEncoding, true, true},
	{base64.RawURLEncoding, true, false},
}

// decodeB64 decodes base64 in any RFC 4648 form, standard or URL-safe alphabet, padded or not, with
// ASCII whitespace ignored (proto3 JSON accepts every form for bytes). The
// exact form is not recorded: the stub's enc stays "base64" or "dataurl".
func decodeB64(x string) ([]byte, bool) {
	if strings.ContainsAny(x, " \t\r\n") {
		x = strings.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, x)
	}
	if x == "" {
		return nil, false
	}
	std, url := strings.ContainsAny(x, "+/"), strings.ContainsAny(x, "-_")
	padded := strings.HasSuffix(x, "=")
	for _, e := range b64Encodings {
		if (e.url && std) || (!e.url && url) || (e.pads != padded && (padded || len(x)%4 != 0)) {
			continue
		}
		if b, err := e.enc.DecodeString(x); err == nil {
			return b, true
		}
	}
	return nil, false
}

// parseDataURL splits data:<mime>[;params];base64,<data>. Only base64 data URLs qualify.
func parseDataURL(x string) (mime, data string, ok bool) {
	if len(x) < 5 || !strings.EqualFold(x[:5], "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(x, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := x[5:comma]
	if len(meta) < 7 || !strings.EqualFold(meta[len(meta)-7:], ";base64") {
		return "", "", false
	}
	mime = meta[:len(meta)-7]
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return mime, x[comma+1:], true
}

// dataURLStub returns the stub for a base64 data URL whose decoded bytes are attachable.
func (s *splitter) dataURLStub(x string) (*node, bool) {
	mime, data, ok := parseDataURL(x)
	if !ok {
		return nil, false
	}
	b, ok := decodeB64(data)
	if !ok || !s.attachable(b, mime) {
		return nil, false
	}
	return s.attach(b, mime, "dataurl"), true
}

// b64Stub returns the stub for bare base64 whose decoded bytes are attachable.
func (s *splitter) b64Stub(x, mime string) (*node, bool) {
	b, ok := decodeB64(x)
	if !ok || !s.attachable(b, mime) {
		return nil, false
	}
	return s.attach(b, mime, "base64"), true
}

// dataOrB64Stub handles fields that hold either a data URL or bare base64.
func (s *splitter) dataOrB64Stub(x, mime string) (*node, bool) {
	if _, _, ok := parseDataURL(x); ok {
		return s.dataURLStub(x)
	}
	return s.b64Stub(x, mime)
}

// stubField replaces o[k] when it is a string the stub function accepts.
func stubField(o *node, k string, stub func(string) (*node, bool)) {
	if v := o.get(k); v.isString() {
		if st, ok := stub(v.s); ok {
			o.set(k, st)
		}
	}
}

// finish assembles the Split.
func (s *splitter) finish(sp *Split) (*Split, error) {
	if s.err != nil {
		return nil, s.err
	}
	sp.Skeleton, sp.Tools = s.skel, s.tools
	if s.toolsV != nil {
		sp.ToolsV = s.toolsV
	}
	sp.System, sp.Items = s.sys.items, s.items.items
	sp.refs = splitRefs{skel: s.skelRefs, tools: s.toolsRefs, sys: s.sys.refs, items: s.items.refs}
	for f := range s.flags {
		sp.Flags = append(sp.Flags, f)
	}
	slices.Sort(sp.Flags)
	sp.Hits, sp.Oversize, sp.MediaURLs = s.hits, s.oversize, s.mediaURLs
	for _, h := range sp.Refs(0) {
		if p, ok := s.pieces[h]; ok {
			sp.Pieces = append(sp.Pieces, p)
		}
	}
	return sp, nil
}
