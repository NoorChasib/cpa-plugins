package importer

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// extTypes declares the content type of saved binary tool outputs by extension. A fixed table, so every
// machine declares the same type (mime.TypeByExtension reads system files); anything else is sniffed
// by pkg/canon.
var extTypes = map[string]string{
	".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".json": "application/json", ".txt": "text/plain",
	".bin": "application/octet-stream",
}

// fileBody builds the import.file envelope body of one whole file: workflow runs and agent
// meta files as masked JSON pieces, scripts, skills and saved tool outputs as masked text pieces, and
// bytes that are not UTF-8 as an attachment through pkg/canon's attachment rules. A workflow script a
// run in the same session embeds byte for byte, and a skill that is not UTF-8 text, produce nothing.
func (b *builder) fileBody(f fileRef, data []byte, embedded func(fileRef) map[[32]byte]bool) (*built, error) {
	var p canon.Piece
	var err error
	switch f.Role {
	case FileWorkflowScript:
		if embedded(f)[sha256.Sum256(data)] {
			return nil, errSkip
		}
		p, err = b.textOrAttachment(f, data)
	case FileSkill:
		if !utf8.Valid(data) || len(data) > maxSkillBytes {
			return nil, errSkip
		}
		p, err = b.textPiece(string(data))
	case FileWorkflowRun, FileAgentMeta:
		if json.Valid(data) {
			p, err = b.jsonPiece(data)
		} else {
			p, err = b.textOrAttachment(f, data)
		}
	default:
		p, err = b.textOrAttachment(f, data)
	}
	if err != nil {
		return nil, err
	}
	out := &built{kind: wire.KindImportFile, createdAt: mtimeOf(f)}
	if p.Bytes == nil && p.Hash == (wire.Hash{}) {
		// An attachment over limits.attachment: canon keeps a reference only, so there is no piece to
		// send. The file becomes a local rejected_too_large dead letter.
		out.key = wire.ImportFileKey(f.Client, f.Key, sha256.Sum256(data))
		out.tooLarge = true
		return out, nil
	}
	body, err := canon.Marshal(wire.ImportFile{Client: f.Client, FileKey: f.Key, Content: p.Hash, Kind: f.Role})
	if err != nil {
		return nil, err
	}
	out.key = wire.ImportFileKey(f.Client, f.Key, p.Hash)
	out.body, out.refs, out.pieces = body, []wire.Hash{p.Hash}, []canon.Piece{p}
	out.markTooLarge(b.overLimits(out))
	return out, nil
}

// errRead marks a whole file that could not be read (it vanished or is unreadable): it produces nothing.
var errRead = errors.New("whole file unreadable")

// readWhole reads and builds one whole file, returning the bytes read. A file larger than any piece the
// archive takes (limits.text_piece and limits.attachment) is never held in memory or
// masked: it is hashed as it streams by and becomes a local rejected_too_large dead letter keyed by the
// sha256 of its bytes, as fileBody keys an oversize attachment.
func (b *builder) readWhole(f fileRef, rd io.Reader, embedded func(fileRef) map[[32]byte]bool) (*built, int64, error) {
	if f.Size > max(b.limits.TextPiece, b.limits.Attachment) {
		h := sha256.New()
		n, err := io.Copy(h, rd)
		if err != nil {
			return nil, n, errRead
		}
		out := &built{kind: wire.KindImportFile, key: wire.ImportFileKey(f.Client, f.Key, wire.Hash(h.Sum(nil))), createdAt: mtimeOf(f)}
		out.markTooLarge(true)
		return out, n, nil
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, int64(len(data)), errRead
	}
	bl, err := b.fileBody(f, data, embedded)
	return bl, int64(len(data)), err
}

// textOrAttachment: valid UTF-8 is one masked text piece; other bytes go through canon's opaque split,
// which makes them one attachment (masking the valid UTF-8 runs of a declared text type).
func (b *builder) textOrAttachment(f fileRef, data []byte) (canon.Piece, error) {
	if utf8.Valid(data) {
		return b.textPiece(string(data))
	}
	ct := extTypes[strings.ToLower(path.Ext(f.Rel))]
	sp, err := canon.SplitRequest(b.ctx, canon.Input{Format: canon.FormatOpaque, Body: data, ContentType: ct}, b.splitOpts())
	if err != nil {
		return canon.Piece{}, err
	}
	b.stats.addHits(sp.Hits)
	if len(sp.Pieces) == 0 {
		return canon.Piece{}, nil
	}
	return sp.Pieces[0], nil
}

// embeddedScripts returns the sha256 of every inline `script` of the workflow runs in f's session
// (a script file is imported only when no run embeds the same bytes). Cached per run.
func (r *runner) embeddedScripts(f fileRef) map[[32]byte]bool {
	parts := strings.SplitN(f.Rel, "/", 3)
	if len(parts) < 3 {
		return nil
	}
	dir := filepath.Join(r.cfg.Roots[f.Root], parts[0], parts[1], "workflows")
	if s, ok := r.scripts[dir]; ok {
		return s
	}
	set := map[[32]byte]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "wf_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var run struct {
			Script *string `json:"script"`
		}
		if json.Unmarshal(data, &run) == nil && run.Script != nil {
			set[sha256.Sum256([]byte(*run.Script))] = true
		}
	}
	r.scripts[dir] = set
	return set
}
