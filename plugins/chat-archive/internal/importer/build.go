package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// leafMin is the size from which a string leaf of a frame becomes its own text piece: Codex records
// other than response items (their large string leaves become text pieces) and `requeue --split`.
// It equals canon.MinAttachment, the split's own small-payload threshold.
const leafMin = canon.MinAttachment

// Modes of an import record (wire.ImportRecord.Mode).
const (
	ModeFull     = "full"
	ModeMetadata = "metadata"
)

// errSkip marks a line that produces no envelope (blank, not a JSON object, a forked rollout's second
// session_meta, a script a workflow run embeds).
var errSkip = errors.New("skip")

// builder turns records and files into envelope bodies with one masker (the same pkg/mask and pkg/canon
// as the plugin, so equal blocks hash equal).
type builder struct {
	ctx    context.Context
	m      *mask.Masker
	memo   *memo
	limits wire.Limits
	split  bool // requeue --split: frames of Claude records also move large string leaves to pieces
	stats  *runStats
}

// built is one envelope body before a seq is assigned.
type built struct {
	kind      wire.Kind
	key       string
	body      []byte
	refs      []wire.Hash
	pieces    []canon.Piece
	createdAt time.Time
	tooLarge  bool
	// fitFrom is the key the record had before fitRecord moved frame subtrees into pieces (empty when
	// nothing moved). A dead row written when the record could not be fitted carries that key.
	fitFrom string
}

// keyedAs reports whether k is the record's key, or its key from before the fit: either proves the
// record was built from the same bytes.
func (o *built) keyedAs(k string) bool {
	return o.key == k || o.fitFrom != "" && o.fitFrom == k
}

// acc collects the items, pieces and refs of one record.
type acc struct {
	b      *builder
	items  []wire.Item
	pieces []canon.Piece
	refs   []wire.Hash
	seen   map[wire.Hash]bool
}

func (b *builder) newAcc() *acc { return &acc{b: b, seen: map[wire.Hash]bool{}} }

func (a *acc) ref(h wire.Hash) {
	if !a.seen[h] {
		a.seen[h] = true
		a.refs = append(a.refs, h)
	}
}

func (a *acc) addPiece(p canon.Piece) wire.Hash {
	if !a.seen[p.Hash] {
		a.pieces = append(a.pieces, p)
		a.ref(p.Hash)
	}
	return p.Hash
}

// addSplit appends a split's items, pieces and refs (Refs(0) covers $att stubs nested in json pieces).
func (a *acc) addSplit(sp *canon.Split) {
	a.items = append(a.items, sp.Items...)
	byHash := make(map[wire.Hash]canon.Piece, len(sp.Pieces))
	for _, p := range sp.Pieces {
		byHash[p.Hash] = p
	}
	for _, h := range sp.Refs(0) {
		if p, ok := byHash[h]; ok {
			a.addPiece(p)
		}
	}
	a.b.stats.addHits(sp.Hits)
}

// maskString masks one string through the per-run memo.
func (b *builder) maskString(s string) (string, error) {
	return b.memo.mask(b.ctx, b.m, s, b.stats)
}

// textPiece returns the masked text piece of s.
func (b *builder) textPiece(s string) (canon.Piece, error) {
	out, err := b.maskString(s)
	if err != nil {
		return canon.Piece{}, err
	}
	return canon.NewPiece(wire.PieceText, "", []byte(out)), nil
}

// jsonPiece returns the masked canon/v1 json piece of raw.
func (b *builder) jsonPiece(raw []byte) (canon.Piece, error) {
	out, hits, _, err := canon.CanonicalizeMasked(raw, b.m)
	if err != nil {
		return canon.Piece{}, err
	}
	b.stats.addMasked(int64(len(raw)), hits)
	return canon.NewPiece(wire.PieceJSON, "", out), nil
}

// frame returns the masked canon/v1 bytes of a frame value.
func (b *builder) frame(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, hits, _, err := canon.CanonicalizeMasked(raw, b.m)
	if err != nil {
		return nil, err
	}
	b.stats.addMasked(int64(len(raw)), hits)
	return out, nil
}

// pathItem appends an item for a piece taken out of the frame: F = {"path":"<JSON pointer>"} into the
// record (RFC 6901, keys as masked in the frame), P = the piece.
func (a *acc) pathItem(ptr string, p canon.Piece) error {
	f, err := canon.Marshal(map[string]string{"path": ptr})
	if err != nil {
		return err
	}
	a.items = append(a.items, wire.Item{F: f, P: ptrHash(a.addPiece(p))})
	return nil
}

func ptrHash(h wire.Hash) *wire.Hash { return &h }

func (a *acc) pathText(ptr, s string) error {
	p, err := a.b.textPiece(s)
	if err != nil {
		return err
	}
	return a.pathItem(ptr, p)
}

func (a *acc) pathJSON(ptr string, raw []byte) error {
	p, err := a.b.jsonPiece(raw)
	if err != nil {
		return err
	}
	return a.pathItem(ptr, p)
}

// pointer builds a JSON pointer from segments; keys are masked like the frame's keys.
func (b *builder) pointer(segs ...string) string {
	masked := make([]string, len(segs))
	for i, s := range segs {
		masked[i], _ = b.m.String(s)
	}
	return escapePointer(masked...)
}

var pointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

// escapePointer builds an RFC 6901 JSON pointer from segments taken as they are (keys already masked).
func escapePointer(segs ...string) string {
	var sb strings.Builder
	for _, s := range segs {
		sb.WriteByte('/')
		sb.WriteString(pointerEscaper.Replace(s))
	}
	return sb.String()
}

// extractLeaves walks a decoded value and moves every string of at least leafMin bytes into a text
// piece: an object member is deleted, an array element becomes null, and a path item records where it
// was. Object keys are visited in sorted order, so the item order is deterministic.
func (a *acc) extractLeaves(v any, path []string) (any, error) {
	switch x := v.(type) {
	case string:
		if len(x) >= leafMin {
			if err := a.pathText(a.b.pointer(path...), x); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return x, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if s, ok := x[k].(string); ok && len(s) >= leafMin {
				if err := a.pathText(a.b.pointer(append(path, k)...), s); err != nil {
					return nil, err
				}
				delete(x, k)
				continue
			}
			nv, err := a.extractLeaves(x[k], append(path, k))
			if err != nil {
				return nil, err
			}
			x[k] = nv
		}
		return x, nil
	case []any:
		for i := range x {
			nv, err := a.extractLeaves(x[i], append(path, strconv.Itoa(i)))
			if err != nil {
				return nil, err
			}
			x[i] = nv
		}
		return x, nil
	}
	return v, nil
}

// decodeAny decodes JSON keeping number literals (json.Number).
func decodeAny(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// metadataOnly turns a, built for a full record, into its metadata form (frames, timings, ids and
// steering; no content items or extra pieces): every item keeps M and F, loses
// P and V, and refs keep only attachments referenced by $att stubs in the kept frames. It reports
// whether anything was dropped; when nothing was, the record stays full, so one key always has one body.
func (a *acc) metadataOnly() bool {
	dropped := false
	keep := map[wire.Hash]bool{}
	for i := range a.items {
		if a.items[i].P != nil || len(a.items[i].V) > 0 {
			dropped = true
		}
		a.items[i].P, a.items[i].V = nil, nil
		for _, raw := range [][]byte{a.items[i].M, a.items[i].F} {
			for _, h := range attRefs(raw) {
				keep[h] = true
			}
		}
	}
	if !dropped {
		return false
	}
	var pieces []canon.Piece
	var refs []wire.Hash
	for _, h := range a.refs {
		if keep[h] {
			refs = append(refs, h)
		}
	}
	for _, p := range a.pieces {
		if keep[p.Hash] {
			pieces = append(pieces, p)
		}
	}
	a.refs, a.pieces = refs, pieces
	return true
}

// attRefs returns the sha256 of every non-dropped $att stub in a canon/v1 frame.
func attRefs(raw []byte) []wire.Hash {
	if !bytes.Contains(raw, []byte(`"`+canon.AttKey+`"`)) {
		return nil
	}
	v, err := decodeAny(raw)
	if err != nil {
		return nil
	}
	var out []wire.Hash
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if st, ok := x[canon.AttKey].(map[string]any); ok {
				if _, dropped := st["dropped"]; !dropped {
					if s, ok := st["sha256"].(string); ok {
						if h, err := wire.ParseHash(s); err == nil {
							out = append(out, h)
						}
					}
				}
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// finishRecord computes the key and the canon/v1 body of an import record. A record whose envelope is
// over limits.envelope_json while its pieces are within theirs is fitted (fitRecord) when fitAllowed;
// a record that cannot be fitted keeps the key computed here and becomes a too-large dead letter.
// Records under the limits never reach the fit, so they are built exactly as before it existed.
func (b *builder) finishRecord(rec wire.ImportRecord, a *acc, key func(sha string) string, createdAt time.Time) (*built, error) {
	out, err := b.encodeRecord(rec, a, key, createdAt)
	if err != nil {
		return nil, err
	}
	if b.overEnvelope(out) && !b.overPieces(out) && fitAllowed(rec) {
		fit, err := b.fitRecord(rec, a, key, createdAt, out)
		if err != nil {
			return nil, err
		}
		if fit != nil {
			fit.fitFrom = out.key
			return fit, nil
		}
	}
	out.markTooLarge(b.overLimits(out))
	return out, nil
}

// encodeRecord returns rec with a's items as a built: its key and canon/v1 body.
func (b *builder) encodeRecord(rec wire.ImportRecord, a *acc, key func(sha string) string, createdAt time.Time) (*built, error) {
	rec.Items = a.items
	sha := wire.RecordKeyHash(rec.Frame, canon.ItemIDs(rec.Items), rec.Extra)
	body, err := canon.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return &built{kind: wire.KindImportRecord, key: key(sha), body: body, refs: a.refs, pieces: a.pieces, createdAt: createdAt}, nil
}

// fitAllowed: every import record but a Codex response_item. Its frame holds no content (the Responses
// rules already split the payload into items), and the archive's Responses grouping would read a path
// item there as a message item.
func fitAllowed(rec wire.ImportRecord) bool {
	return rec.Client != ClientCodex || rec.RecordType != "response_item" && !strings.HasPrefix(rec.RecordType, "response_item/")
}

// fitRecord brings a record whose envelope is over limits.envelope_json under it by moving whole
// objects and arrays out of its masked frame into json pieces. Each moved node becomes a path item
// appended after the record's items, the same item the leaf rule writes: an object member is removed
// from the frame, an array element becomes null. Each pass moves the node takeSubtree picks and
// measures again. A subtree is a slice of the masked canon/v1 frame, so its piece is not masked again.
// Applying the path items to the frame in reverse order gives back the masked record: a later move can
// hold an earlier one, never the other way round.
//
// It works on copies of a's items, pieces and refs, so first (the record as built) is untouched. It
// returns nil when no object or array of at least leafMin bytes is left to move, or when a piece would
// be over its limit.
func (b *builder) fitRecord(rec wire.ImportRecord, a *acc, key func(sha string) string, createdAt time.Time, first *built) (*built, error) {
	root, err := decodeAny(rec.Frame)
	if err != nil {
		return nil, err
	}
	fa := &acc{b: b, items: slices.Clone(a.items), pieces: slices.Clone(a.pieces), refs: slices.Clone(a.refs), seen: maps.Clone(a.seen)}
	cur := first
	for b.overEnvelope(cur) {
		segs, v, err := takeSubtree(root, envelopeSize(cur)-b.limits.EnvelopeJSON+leafMin)
		if err != nil || segs == nil {
			return nil, err
		}
		raw, err := canon.Marshal(v)
		if err != nil {
			return nil, err
		}
		if err := fa.pathItem(escapePointer(segs...), canon.NewPiece(wire.PieceJSON, "", raw)); err != nil {
			return nil, err
		}
		if rec.Frame, err = canon.Marshal(root); err != nil {
			return nil, err
		}
		if cur, err = b.encodeRecord(rec, fa, key, createdAt); err != nil {
			return nil, err
		}
		if b.overPieces(cur) {
			return nil, nil
		}
	}
	return cur, nil
}

// takeSubtree removes the node one fit pass moves from the decoded frame and returns its pointer
// segments and value, or nil segments when the root has no object or array child of at least leafMin
// bytes. need is the number of bytes the envelope must lose, plus leafMin for the path item and its
// ref. From the root it steps into the heaviest child (heaviest) while that child alone covers need,
// and the node where it stops moves: the deepest node that is enough, which keeps small fields such as
// type and ids in the frame. When no child of the root covers need, the root's heaviest child moves
// and the next pass measures again.
func takeSubtree(root any, need int64) ([]string, any, error) {
	seg, node, size, err := heaviest(root)
	if err != nil || node == nil {
		return nil, nil, err
	}
	segs, parent := []string{seg}, root
	for size >= need {
		s, c, n, err := heaviest(node)
		if err != nil {
			return nil, nil, err
		}
		if c == nil || n < need {
			break
		}
		segs, parent, node, size = append(segs, s), node, c, n
	}
	switch p := parent.(type) {
	case map[string]any:
		delete(p, segs[len(segs)-1])
	case []any:
		i, err := strconv.Atoi(segs[len(segs)-1])
		if err != nil {
			return nil, nil, err
		}
		p[i] = nil
	}
	return segs, node, nil
}

// heaviest returns n's object or array child with the longest canon/v1 encoding of at least leafMin
// bytes: its pointer segment, value and size. Keys are visited in byte order and elements by index,
// and a tie keeps the first, so the choice never depends on map iteration order.
func heaviest(n any) (seg string, child any, size int64, err error) {
	consider := func(s string, c any) error {
		switch c.(type) {
		case map[string]any, []any:
		default:
			return nil
		}
		raw, err := canon.Marshal(c)
		if err != nil {
			return err
		}
		if l := int64(len(raw)); l >= leafMin && l > size {
			seg, child, size = s, c, l
		}
		return nil
	}
	switch x := n.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := consider(k, x[k]); err != nil {
				return "", nil, 0, err
			}
		}
	case []any:
		for i, e := range x {
			if err := consider(strconv.Itoa(i), e); err != nil {
				return "", nil, 0, err
			}
		}
	}
	return seg, child, size, nil
}

// markTooLarge sets tooLarge and, when set, drops the body, refs and pieces at once: a too-large unit
// is never sent (only its key and kind reach the dead row and tombstone), so a batch never holds them.
func (o *built) markTooLarge(over bool) {
	o.tooLarge = over
	if over {
		o.body, o.refs, o.pieces = nil, nil, nil
	}
}

// overLimits reports whether the envelope or one of its pieces is over the server's limits (sources/self),
// which makes it a local rejected_too_large dead letter.
func (b *builder) overLimits(o *built) bool {
	return b.overEnvelope(o) || b.overPieces(o)
}

// envelopeSize estimates the envelope of o: its body, 67 bytes a ref, and 1 KiB for the fields outside
// the body.
func envelopeSize(o *built) int64 {
	return int64(len(o.body)) + int64(len(o.refs))*67 + 1024
}

// overEnvelope reports whether the envelope estimate is over limits.envelope_json.
func (b *builder) overEnvelope(o *built) bool {
	return envelopeSize(o) > b.limits.EnvelopeJSON
}

// overPieces reports whether a json or text piece is over limits.text_piece, or an attachment over
// limits.attachment.
func (b *builder) overPieces(o *built) bool {
	for _, p := range o.pieces {
		limit := b.limits.TextPiece
		if p.Kind == wire.PieceAttachment {
			limit = b.limits.Attachment
		}
		if int64(len(p.Bytes)) > limit {
			return true
		}
	}
	return false
}

// keyPart keeps a key segment inside the key alphabet (wire.ValidKey): any other byte becomes '_', and a
// segment over 64 bytes becomes 'h' plus 32 hex of its sha256. Real ids (uuids, kebab-case types) pass
// unchanged.
func keyPart(s string) string {
	if s == "" {
		return "-"
	}
	ok := len(s) <= 64
	for i := 0; ok && i < len(s); i++ {
		c := s[i]
		ok = 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("_.:@-", c) >= 0
	}
	if ok {
		return s
	}
	if len(s) > 64 {
		sum := sha256.Sum256([]byte(s))
		return "h" + fmt.Sprintf("%x", sum[:16])
	}
	bs := []byte(s)
	for i, c := range bs {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("_.:@-", c) >= 0) {
			bs[i] = '_'
		}
	}
	return string(bs)
}

// jsonStr returns raw as a string when it is a JSON string.
func jsonStr(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// parseTS parses an RFC 3339 timestamp field.
func parseTS(raw json.RawMessage) *time.Time {
	s := jsonStr(raw)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// createdAt is the envelope's created_at: the record's own timestamp, else the file's mtime.
func createdAt(ts *time.Time, f fileRef) time.Time {
	if ts != nil {
		return *ts
	}
	return mtimeOf(f)
}
