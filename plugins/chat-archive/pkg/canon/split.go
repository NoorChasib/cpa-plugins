package canon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Input is one raw body as a hook (or the importer) saw it: its format and bytes, plus the content
// type (multipart boundary, speech mime) and content encoding (gzip) that non-JSON bodies need.
type Input struct {
	Format          string // a Format* value
	Body            []byte
	ContentType     string // Content-Type of the body: multipart boundary, speech mime
	ContentEncoding string // Content-Encoding of the body: "gzip" is decoded first, capped at Options.MaxDecoded (flag gunzipped); "" and "identity" mean none; anything else is ErrUndecodableBody
}

// Memo caches Masker.String results across bodies: the plugin's masking memo LRU and the
// importer's per-run memo. Keys are sha256 of the unmasked string. Implementations live in
// memory only, are safe for concurrent use, and are cleared by their owner when exact_rev changes.
type Memo interface {
	Get(key wire.Hash) (masked string, hits mask.Hits, ok bool)
	Add(key wire.Hash, masked string, hits mask.Hits)
}

// Options configure one split.
type Options struct {
	Masker        *mask.Masker // required: applied to every string value and object key
	MaxAttachment int64        // limits.attachment; 0 means wire.DefaultLimits.Attachment. Larger attachments get "dropped":"oversize" stubs and no piece
	Memo          Memo         // optional; nil disables memoization

	// MaxDecoded caps the decoded size of a gzip body; 0 means MaxGunzip. Over it the split returns
	// ErrBodyBudget after reading at most MaxDecoded+1 decoded bytes, so the plugin can bound a small
	// compressed body that expands far past its reservation.
	MaxDecoded int64
	// MaxNodes caps the JSON values the split parses (every object, array, string, number and
	// literal, the body's and tool arguments' alike); 0 means no cap. Over it the split returns
	// ErrBodyBudget. Each value costs about 72 bytes on top of its string bytes, so a body of many
	// small values needs far more memory than its size (peak heap measured at about 160 bytes per
	// number of a float array and 250 per object member, 18 to 22 times the body).
	MaxNodes int
}

// MediaURL is one absolute http/https URL found in a media.* response body (a provider
// media URL). URL is unmasked: it lives in memory and in wire.MediaFetch only, never in a piece, the outbox
// or a log.
type MediaURL struct {
	URL       string
	MaskedURL string // headers.StripURL(URL, m): the string left in the body
}

// Split is the structural split of one body: Skeleton, Tools, ToolsV, System, Items and
// every piece and attachment they reference.
type Split struct {
	Format    string          // the Body.Format actually applied: FormatMultipart for a multipart/* Content-Type, FormatOpaque when the body did not parse as Input.Format
	RawLen    int64           // len(Input.Body) as received
	Skeleton  *wire.Hash      // json piece: body minus system, messages/input/contents and tools
	Tools     *wire.Hash      // json piece: tools array with per-tool cache_control removed
	ToolsV    json.RawMessage // [{"i":<index>,"v":<removed fields>}], masked
	System    []wire.Item     // system units
	Items     []wire.Item     // message frames and blocks, in order
	Pieces    []Piece         // every piece and attachment referenced, deduplicated, in first-reference order; no oversize attachments
	Flags     []string        // wire.Flag* values raised by the split
	Hits      mask.Hits       // masking counts per rule id over the whole body
	Oversize  int             // attachments kept as reference only ("dropped":"oversize")
	MediaURLs []MediaURL      // media.* responses only; in body order (the MediaFetch ordinal is the index)

	refs splitRefs // per-unit reference lists for Refs
}

// Body returns the wire.Body of s with all items: Base nil, N = len(Items),
// ListSHA = ListSHA(ItemIDs(Items)). The plugin applies the prefix delta afterwards
// (sets Base, trims Items).
func (s *Split) Body() wire.Body {
	return wire.Body{
		Format:   s.Format,
		RawLen:   s.RawLen,
		Skeleton: s.Skeleton,
		Tools:    s.Tools,
		ToolsV:   s.ToolsV,
		System:   s.System,
		Items:    s.Items,
		N:        uint32(len(s.Items)),
		ListSHA:  ListSHA(ItemIDs(s.Items)),
		Flags:    s.Flags,
	}
}

// Refs returns every piece and attachment hash an envelope body built from s references when its Items
// start at item from (from = Base.Kept, 0 without a base): Skeleton, Tools, every System item, and
// Items[from:] — payload pieces and attachments, including $att stubs nested inside json pieces, but no
// oversize-dropped attachment. Deduplicated, in first-reference order. This is Envelope.Refs.
func (s *Split) Refs(from int) []wire.Hash {
	seen := make(map[wire.Hash]bool)
	var out []wire.Hash
	add := func(hs []wire.Hash) {
		for _, h := range hs {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	add(s.refs.skel)
	add(s.refs.tools)
	for _, r := range s.refs.sys {
		add(r)
	}
	for i := max(from, 0); i < len(s.refs.items); i++ {
		add(s.refs.items[i])
	}
	return out
}

// SplitRequest splits a request body with the per-format block payload rules, in this order: parse
// once with literal numbers, remove volatile fields to V/ToolsV, decode attachments, stub opaque fields,
// mask every string and key (V and ToolsV included), canonicalize, hash. System units: Claude system
// blocks; chat/Responses system and developer messages stay ordinary items and Responses instructions
// become one text system item; Gemini systemInstruction.parts. Non-JSON bodies are split as
// multipart or kept opaque. It returns ctx.Err() if ctx ends first (the plugin's masking deadline),
// ErrUndecodableBody for a body still content-encoded and ErrBodyBudget for a body over
// Options.MaxDecoded or Options.MaxNodes.
func SplitRequest(ctx context.Context, in Input, opts Options) (*Split, error) {
	if opts.Masker == nil {
		return nil, errNoMasker
	}
	return newSplitter(ctx, opts.Masker, stripper(opts.Masker), opts).split(in, false)
}

func stripper(m *mask.Masker) func(string) string {
	return func(u string) string { return headers.StripURL(u, m) }
}

// split runs one request or response split.
func (s *splitter) split(in Input, response bool) (*Split, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	sp := &Split{Format: in.Format, RawLen: int64(len(in.Body))}
	body := in.Body
	switch enc := strings.ToLower(strings.TrimSpace(in.ContentEncoding)); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		b, err := gunzip(body, s.maxDec)
		if err != nil {
			return nil, err
		}
		body = b
		s.flag(wire.FlagGunzipped)
	default:
		return nil, fmt.Errorf("%w: unsupported Content-Encoding", ErrUndecodableBody)
	}
	if boundary, ok := multipartBoundary(in.ContentType); ok {
		if parts, err := readMultipart(body, boundary); err == nil {
			sp.Format = FormatMultipart
			s.multipart(parts)
			return s.finish(sp)
		}
		sp.Format = FormatOpaque
		s.opaqueRaw(body, in.ContentType)
		return s.finish(sp)
	}
	media := strings.HasPrefix(in.Format, FormatMediaPrefix) && len(in.Format) > len(FormatMediaPrefix)
	n, p, err := s.parse(body)
	if errors.Is(err, ErrBodyBudget) {
		return nil, err
	}
	if err != nil {
		if media && response { // speech audio and other raw media bodies: one attachment
			s.startUnit()
			s.frameOnly(&s.items, s.attach(body, in.ContentType, "raw"))
			return s.finish(sp)
		}
		sp.Format = FormatOpaque
		s.opaqueRaw(body, in.ContentType)
		return s.finish(sp)
	}
	s.parseFlags(p)
	switch {
	case media:
		s.media(n, response)
		return s.finish(sp)
	case !n.isObject() || !knownJSONFormat(in.Format):
		sp.Format = FormatOpaque
		s.startUnit()
		s.skeletonPiece(n)
		return s.finish(sp)
	}
	switch in.Format {
	case FormatClaude:
		if response {
			s.claudeResponse(n)
		} else {
			s.claudeRequest(n)
		}
	case FormatOpenAIChat:
		if response {
			s.chatResponse(n)
		} else {
			s.chatRequest(n)
		}
	case FormatOpenAIResponses:
		if response {
			s.responsesTop(n, "output")
		} else {
			s.responsesTop(n, "input")
		}
	case FormatGemini:
		if response {
			s.geminiResponse(n)
		} else {
			s.geminiRequest(n)
		}
	case FormatInteractions:
		if response {
			s.interactionsResponse(n)
		} else {
			s.interactionsRequest(n)
		}
	}
	return s.finish(sp)
}

func knownJSONFormat(f string) bool {
	switch f {
	case FormatClaude, FormatOpenAIChat, FormatOpenAIResponses, FormatGemini, FormatInteractions:
		return true
	}
	return false
}

// gunzip decodes a gzip body of at most maxOut decoded bytes. A stream
// that does not decode is ErrUndecodableBody (its bytes are never stored, so no compressed plaintext
// reaches a blob); more than maxOut decoded bytes is ErrBodyBudget, found after reading at most
// maxOut+1. The buffer is sized from the gzip trailer's ISIZE when that is at most maxOut, so an honest
// stream is decoded into one allocation; otherwise it doubles, never past maxOut+1.
func gunzip(b []byte, maxOut int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %v", ErrUndecodableBody, err)
	}
	hint := int64(64 << 10)
	if len(b) >= 4 {
		if isize := int64(binary.LittleEndian.Uint32(b[len(b)-4:])); isize <= maxOut {
			hint = isize
		}
	}
	buf := make([]byte, 0, min(hint, maxOut)+1)
	for {
		if len(buf) == cap(buf) {
			next := min(max(2*int64(cap(buf)), 64<<10), maxOut+1)
			if next <= int64(cap(buf)) {
				return nil, ErrBodyBudget
			}
			buf = append(make([]byte, 0, next), buf...)
		}
		n, err := zr.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if int64(len(buf)) > maxOut {
			return nil, ErrBodyBudget
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: gzip: %v", ErrUndecodableBody, err)
		}
	}
}

// opaqueRaw stores an unparsable body as one text piece when it is valid UTF-8, else as one attachment.
func (s *splitter) opaqueRaw(body []byte, contentType string) {
	s.startUnit()
	if utf8.Valid(body) {
		text := string(body)
		s.finishBlock(&s.items, typeNode("text"), &text, nil, nil)
		return
	}
	s.frameOnly(&s.items, s.attach(body, contentType, "raw"))
}

func multipartBoundary(contentType string) (string, bool) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mt, "multipart/") || params["boundary"] == "" {
		return "", false
	}
	return params["boundary"], true
}

type formPart struct {
	name, filename, contentType string
	hasFilename, file           bool
	data                        []byte
}

// readMultipart parses the whole form first, so a malformed body leaves no partial split behind.
func readMultipart(body []byte, boundary string) ([]formPart, error) {
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	var parts []formPart
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return parts, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, err
		}
		fp := formPart{name: p.FormName(), contentType: p.Header.Get("Content-Type"), data: data}
		if _, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition")); err == nil {
			fp.filename, fp.hasFilename = params["filename"]
		}
		fp.file = fp.hasFilename || fp.contentType != ""
		parts = append(parts, fp)
	}
}

// multipart: each file part is one attachment item, {"data":{"$att":…},"filename":…,"name":…,
// "type":"file"}; the text fields are one json piece, the Skeleton.
func (s *splitter) multipart(parts []formPart) {
	fields := objNode()
	for _, p := range parts {
		name := s.validString(p.name)
		if !p.file {
			if fields.has(name) {
				s.flag(wire.FlagDupKeys)
			}
			fields.set(name, strNode(s.validString(string(p.data))))
			continue
		}
		s.startUnit()
		f := objNode()
		f.set("data", s.attach(p.data, p.contentType, "raw"))
		if p.hasFilename {
			f.set("filename", strNode(s.validString(p.filename)))
		}
		f.set("name", strNode(name))
		f.set("type", strNode("file"))
		s.frameOnly(&s.items, f)
	}
	s.startUnit()
	s.skeletonPiece(fields)
}

// validString applies the parser's U+FFFD rule to a string that did not come from JSON.
func (s *splitter) validString(x string) string {
	out, ok := fixUTF8(x)
	if !ok {
		s.flag(wire.FlagBadUTF8)
	}
	return out
}

// SplitResponse splits a response object (non-stream body, or a stream reassembled into the equivalent
// non-stream object) with the same item rules: Claude one assistant message; Responses the ordered
// output items; chat choices[0].message (other choices as alternates in Skeleton); Gemini
// candidates[0].content. media.* formats first replace every absolute URL by StripMediaURLs and report
// them in Split.MediaURLs; gzip bodies are gunzipped; speech bodies become one attachment. Errors as
// SplitRequest.
func SplitResponse(ctx context.Context, in Input, opts Options) (*Split, error) {
	if opts.Masker == nil {
		return nil, errNoMasker
	}
	return newSplitter(ctx, opts.Masker, stripper(opts.Masker), opts).split(in, true)
}

// SplitMessages splits a JSON array of messages (FormatClaude message objects) or items
// (FormatOpenAIResponses input/output items) with the same per-message block rules as SplitRequest's
// Items, for transcript records (ImportRecord.Items). Only Items, Pieces, Flags, Hits
// and Oversize are set.
func SplitMessages(ctx context.Context, format string, messages json.RawMessage, opts Options) (*Split, error) {
	if opts.Masker == nil {
		return nil, errNoMasker
	}
	return newSplitter(ctx, opts.Masker, nil, opts).messages(format, messages)
}

func (s *splitter) messages(format string, messages []byte) (*Split, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if format != FormatClaude && format != FormatOpenAIResponses {
		return nil, fmt.Errorf("canon: SplitMessages: unsupported format %q", format)
	}
	n, p, err := s.parse(messages)
	if errors.Is(err, ErrBodyBudget) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("canon: SplitMessages: %w", err)
	}
	if !n.isArray() {
		return nil, errors.New("canon: SplitMessages: messages must be a JSON array")
	}
	s.parseFlags(p)
	for _, m := range n.elems {
		if format == FormatClaude {
			s.claudeMessage(&s.items, m)
		} else {
			s.responsesItem(&s.items, m)
		}
	}
	return s.finish(&Split{})
}

// StripMediaURLs replaces every string value in a JSON body that parses as an absolute http/https URL
// by headers.StripURL(url, m), and returns the canon/v1 result with the URLs found, in body order.
func StripMediaURLs(body []byte, m *mask.Masker) (stripped []byte, urls []MediaURL, err error) {
	if m == nil {
		return nil, nil, errNoMasker
	}
	return stripMediaURLs(body, stripper(m))
}

func stripMediaURLs(body []byte, strip func(string) string) ([]byte, []MediaURL, error) {
	n, _, err := parse(body)
	if err != nil {
		return nil, nil, err
	}
	urls := stripURLs(n, strip, nil)
	var e encoder
	e.value(n)
	return e.buf, urls, nil
}
