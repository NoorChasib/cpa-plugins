package importer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Run tuning. defaultMaxBytes bounds the new bytes one timer `run` reads (`--max-bytes`; `backfill` has
// no limit). The retry bounds keep one run from looping: what is left goes to the next run.
const (
	defaultMaxBytes = 256 << 20
	maxRounds       = 6 // commit rounds for seqs answered retry
	maxTransient    = 5 // consecutive whole-request failures (429/503/5xx/network) before the run stops
	maxBackoff      = 5 * time.Minute
	missingRefLimit = 3 // missing_ref answers in a row before a local dead letter
	haveCacheMax    = 200_000
	authBackoff     = 15 * time.Minute // no upload after a 401 or 403
	headBytes       = 4 << 10          // head_sha256 covers at most this many leading bytes
	maxBatchUnits   = 20_000
	// runHeartbeatEvery spaces the heartbeats a long run sends before its last one (state partial). A
	// first backfill can take hours, and the archive's importer dead-man fires after 60 minutes of
	// silence, while a timer `run` that finds the lock busy exits without a heartbeat of its own.
	runHeartbeatEvery = 5 * time.Minute
)

// maxHeldBytes bounds the piece bytes one batch holds in memory, pieces sent by PUT included (saved tool
// outputs reach 64 MiB each), so a batch flushes at about this size. A variable
// only so a test can lower it.
var maxHeldBytes int64 = 64 << 20

var (
	errEpoch      = errors.New("archive epoch or archive id changed")
	errAuth       = errors.New("archive answered 401/403: uploads stop for 15 minutes")
	errStop       = errors.New("archive does not accept protocol v1 (server_too_old)")
	errTransient  = errors.New("archive unavailable; the next run retries")
	errIncomplete = errors.New("some records got no receipt; the next run retries them")
)

type runOpts struct {
	session  string
	maxBytes int64
	dryRun   bool
	backfill bool
}

// fileState is one file being read in this run.
type fileState struct {
	ref      fileRef
	cur      Cursor
	has      bool
	fh       *os.File
	scanGen  uint32
	ackEnd   int64 // persisted end_offset
	ackOrd   int64
	ackSeq   uint64
	ackObx   string
	prefix   []byte // the file's first bytes (up to headBytes) as read at open, for head_sha256
	pending  int    // units in the unflushed batch
	complete bool   // read to the end of its complete lines
}

// unitRec is one line (or one whole file) and the envelope it produced.
type unitRec struct {
	fs      *fileState
	ordinal int64
	offset  int64
	length  int64
	end     int64
	b       *built // nil: the line produced no envelope
	seq     uint64
	raw     []byte

	done       bool
	status     wire.Status
	reason     wire.Reason
	deadReason string // dead.reason
	tombReason string // set for local dead letters
	rules      []string
	missing    int
	requeue    *Dead // requeue: the dead row this unit re-sends
}

type tombState struct {
	TombRow
	acked bool
	isNew bool
}

type batch struct {
	units  []*unitRec
	nEnv   int
	bytes  int64 // estimated commit bytes: envelopes and inline pieces (wire.TargetCommitBytes)
	held   int64 // bytes of every distinct piece the batch holds, any size (maxHeldBytes)
	pieces map[wire.Hash]bool
}

// runner is one run, backfill, requeue or dry run.
type runner struct {
	env    *Env
	ctx    context.Context
	paths  Paths
	cfg    Config
	token  string
	st     *State
	meta   Meta
	cl     *client.Client
	self   wire.SourcesSelf
	limits wire.Limits
	b      *builder
	stats  *runStats
	opts   runOpts

	cursors  map[string]Cursor
	have     map[wire.Hash]bool
	tombs    []*tombState
	scripts  map[string]map[[32]byte]bool
	coverage map[string]map[string]bool

	bootID      string
	started     time.Time
	hbN         uint64    // heartbeats sent in this run
	lastHB      time.Time // when the last heartbeat was sent, or the run's start
	mismatch    bool
	limitHit    bool
	hw          uint64
	lastAck     time.Time
	failures    uint32
	lastErr     string
	faults      map[string]bool
	files       []fileRef
	requeueGone int
}

func (e *Env) newRunner(o runOpts) (*runner, error) {
	p := pathsFor(e.Home)
	r := &runner{
		env: e, ctx: context.Background(), paths: p, opts: o, stats: newRunStats(),
		have: map[wire.Hash]bool{}, scripts: map[string]map[[32]byte]bool{}, coverage: map[string]map[string]bool{},
		bootID: randomHex(16), started: e.now(), faults: map[string]bool{}, limits: wire.DefaultLimits,
	}
	r.lastHB = r.started
	cfg, err := loadConfig(p)
	if err != nil {
		return nil, err
	}
	tok, err := loadToken(p)
	if err != nil {
		return nil, err
	}
	r.cfg, r.token = cfg, tok
	st, err := openState(p.StateDB)
	if err != nil {
		return nil, err
	}
	r.st = st
	if m, err := st.meta(); err == nil {
		r.meta = m
	} else if !errors.Is(err, errNoMeta) {
		st.Close()
		return nil, err
	}
	r.cl = client.New(cfg.ArchiveURL, tok, client.Options{
		HTTPClient: e.HTTPClient, UserAgent: "chat-archive-import/" + Version,
		OnServerState: func(s wire.ServerState) {
			if r.meta.ArchiveID != "" && (s.Epoch != r.meta.EpochSeen || s.ArchiveID != r.meta.ArchiveID) {
				r.mismatch = true
			}
		},
	})
	return r, nil
}

func (r *runner) close() { r.st.Close() }

// start fetches the source's view (exact set, limits, epoch, hw), applies an epoch change (epochReset)
// and builds the masker from the exact set plus the fingerprint of the source's own token.
func (r *runner) start() error {
	self, err := r.cl.SourcesSelf(r.ctx)
	if err != nil {
		return r.requestError(err)
	}
	r.self = self
	if self.Limits != (wire.Limits{}) {
		r.limits = self.Limits
	}
	switch {
	case r.meta.OutboxID == "":
		r.meta = Meta{OutboxID: randomHex(16), NextSeq: 1, EpochSeen: self.Epoch, ArchiveID: self.ArchiveID}
		if !r.opts.dryRun {
			if err := r.st.putMeta(r.meta); err != nil {
				return err
			}
		}
	case self.Epoch != r.meta.EpochSeen || self.ArchiveID != r.meta.ArchiveID:
		if !r.opts.dryRun {
			if err := r.epochReset(self); err != nil {
				return err
			}
		}
	}
	r.mismatch = false
	cfg := mask.Config{ExactRev: self.Exact.Rev}
	for _, h := range self.Exact.FPs {
		cfg.ExactFPs = append(cfg.ExactFPs, h)
	}
	cfg.ExactFPs = append(cfg.ExactFPs, fingerprint.ExactFP(r.token))
	for _, h := range self.Exact.CallerFPs {
		cfg.CallerFPs = append(cfg.CallerFPs, h)
	}
	m, err := mask.New(cfg)
	if err != nil {
		return err
	}
	r.b = &builder{ctx: r.ctx, m: m, memo: newMemo(), limits: r.limits, stats: r.stats}
	if r.cursors, err = r.st.cursors(); err != nil {
		return err
	}
	pend, err := r.st.pendingTombstones()
	if err != nil {
		return err
	}
	r.tombs = r.tombs[:0]
	for _, t := range pend {
		r.tombs = append(r.tombs, &tombState{TombRow: t})
	}
	return nil
}

// epochReset handles a new archive epoch or archive id (a restore): a new outbox_id (seq from 1);
// every file whose last committed seq is above its outbox's hw starts again from offset 0 (a rescan, so
// scan_gen grows); pending tombstones of old outboxes are dropped, since their seqs no longer matter.
func (r *runner) epochReset(self wire.SourcesSelf) error {
	curs, err := r.st.cursors()
	if err != nil {
		return err
	}
	reset := 0
	err = r.st.tx(func(t *sql.Tx) error {
		for _, c := range curs {
			if c.LastSeq > self.Outboxes[c.OutboxID] {
				c.Size, c.EndOffset, c.LineOrdinal, c.HeadSHA = -1, 0, 0, ""
				if err := putCursor(t, c); err != nil {
					return err
				}
				reset++
			}
		}
		if _, err := t.Exec(`DELETE FROM tombstones`); err != nil {
			return err
		}
		r.meta.OutboxID, r.meta.NextSeq = randomHex(16), 1
		r.meta.EpochSeen, r.meta.ArchiveID = self.Epoch, self.ArchiveID
		return putMeta(t, r.meta)
	})
	if err == nil {
		fmt.Fprintf(r.env.Stdout, "epoch changed: new outbox, %d files to rescan\n", reset)
	}
	return err
}

// run is one sweep with the epoch rule: an epoch change found mid-run resets and sweeps again once.
func (r *runner) run() error {
	if !r.opts.dryRun && r.meta.AuthBackoffUntil > r.env.now().Unix() {
		fmt.Fprintf(r.env.Stdout, "uploads paused after a 401 until %s\n", time.Unix(r.meta.AuthBackoffUntil, 0).UTC().Format(time.RFC3339))
		return errAuth
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = r.start(); err != nil {
			if errors.Is(err, errEpoch) && attempt == 0 {
				continue
			}
			break
		}
		err = r.sweep()
		if errors.Is(err, errEpoch) && attempt == 0 && !r.opts.dryRun {
			continue
		}
		break
	}
	return err
}

// sweep discovers files and reads every new complete line.
func (r *runner) sweep() error {
	files, err := discover(r.cfg.Roots)
	if err != nil {
		return err
	}
	var sel []fileRef
	for _, f := range files {
		if matchesSession(f, r.opts.session) {
			sel = append(sel, f)
		}
	}
	if r.opts.backfill {
		orderByFirstRecord(sel)
	}
	r.files = sel
	bt := &batch{pieces: map[wire.Hash]bool{}}
	for _, f := range sel {
		if r.limitHit {
			break
		}
		if err := r.processFile(f, bt); err != nil {
			return err
		}
	}
	if err := r.flush(bt); err != nil {
		return err
	}
	return r.flushTombstones()
}

// orderByFirstRecord sorts files by their first record's timestamp (whole files and files without one
// by mtime), so a backfill follows history.
func orderByFirstRecord(files []fileRef) {
	ts := make(map[string]int64, len(files))
	for _, f := range files {
		t := f.MtimeNS
		if !f.Whole {
			if fh, err := openContent(f); err == nil {
				line, _ := bufio.NewReaderSize(fh, 64<<10).ReadBytes('\n')
				fh.Close()
				var rec struct {
					Timestamp string `json:"timestamp"`
				}
				if json.Unmarshal(line, &rec) == nil {
					if p, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
						t = p.UnixNano()
					}
				}
			}
		}
		ts[f.Key] = t
	}
	sort.SliceStable(files, func(i, j int) bool {
		if ts[files[i].Key] != ts[files[j].Key] {
			return ts[files[i].Key] < ts[files[j].Key]
		}
		return files[i].Key < files[j].Key
	})
}

// processFile reads one file from its cursor: unchanged files are skipped; a changed inode,
// a size below end_offset or a changed head starts a rescan from 0 with scan_gen + 1.
func (r *runner) processFile(f fileRef, bt *batch) error {
	cur, has := r.cursors[f.Key]
	if has && cur.Size >= 0 && cur.Inode == f.Inode && cur.Size == f.Size && cur.MtimeNS == f.MtimeNS {
		return nil
	}
	if r.opts.maxBytes > 0 && r.stats.bytesRead >= r.opts.maxBytes {
		// --max-bytes reached (a whole file read last can pass it): this file waits for the next run.
		r.limitHit = true
		return nil
	}
	if f.Zstd {
		return r.processCompressed(f, cur, has, bt)
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil // vanished since discovery
	}
	defer fh.Close()
	r.stats.files++
	fs := &fileState{ref: f, cur: cur, has: has, fh: fh}
	if has {
		fs.ackSeq, fs.ackObx = cur.LastSeq, cur.OutboxID
	}
	fs.prefix = readPrefix(fh, min(f.Size, headBytes))
	if f.Whole {
		return r.processWhole(fs, bt)
	}
	start, ord := int64(0), int64(0)
	if has {
		fs.scanGen = cur.ScanGen
		start, ord = cur.EndOffset, cur.LineOrdinal
		// A Codex rollout keeps its cursor across a new inode: Codex rewrites one only to compress or restore
		// it (zst.go), with the same content, so after a restore the head and size decide, as they do for
		// the compressed form.
		moved := cur.Inode != f.Inode && f.Role != RoleCodexRollout
		if cur.Size < 0 || moved || f.Size < cur.EndOffset || headOf(fs.prefix, cur.EndOffset) != cur.HeadSHA {
			start, ord, fs.scanGen = 0, 0, cur.ScanGen+1
		}
	}
	fs.ackEnd, fs.ackOrd = start, ord
	var cf *codexFile
	if f.Role == RoleCodexRollout {
		if cf, err = r.codexInfo(f); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // compressed or restored by Codex since it was opened: read again next run
			}
			return err
		}
	}
	if _, err := fh.Seek(start, io.SeekStart); err != nil {
		return err
	}
	return r.readLines(fs, cf, bufio.NewReaderSize(fh, 1<<20), start, ord, bt)
}

// processCompressed reads a rollout Codex compressed in place as its decompressed content. The cursor's
// stat (inode, size, mtime) is the .zst file's; its end_offset, line ordinal and head_sha256 are the
// content's. Codex rewrites the file when it compresses it, so the inode and size never match a cursor the
// plain file left: the head alone decides, and a cursor whose head still matches carries over, which
// uploads nothing again. Content shorter than the cursor's end is read again from 0 with scan_gen + 1.
func (r *runner) processCompressed(f fileRef, cur Cursor, has bool, bt *batch) error {
	if f.Size == 0 {
		r.stats.drop("zst_error")
		return nil // not a zstd stream (Codex publishes a finished file): tried again when it changes
	}
	rc, err := openContent(f)
	if err != nil {
		return nil // vanished since discovery, or not a zstd stream: tried again next run
	}
	defer func() {
		if rc != nil {
			rc.Close()
		}
	}()
	r.stats.files++
	fs := &fileState{ref: f, cur: cur, has: has}
	if has {
		fs.ackSeq, fs.ackObx = cur.LastSeq, cur.OutboxID
	}
	// zstReader reports a cut-off stream as errZstDamaged, so io.EOF and io.ErrUnexpectedEOF here mean
	// content shorter than headBytes.
	head := make([]byte, headBytes)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		r.stats.drop("zst_error")
		return nil // a damaged stream: its cursor stays, and the next run tries again
	}
	fs.prefix = head[:n]
	start, ord := int64(0), int64(0)
	if has {
		fs.scanGen = cur.ScanGen
		start, ord = cur.EndOffset, cur.LineOrdinal
		if cur.Size < 0 || headOf(fs.prefix, cur.EndOffset) != cur.HeadSHA {
			start, ord, fs.scanGen = 0, 0, cur.ScanGen+1
		}
	}
	var rd io.Reader = io.MultiReader(bytes.NewReader(fs.prefix), rc)
	if start > 0 {
		if _, err := io.CopyN(io.Discard, rd, start); err != nil {
			// io.CopyN reports content that ends before the cursor as io.EOF; anything else is a damaged
			// stream, which keeps its cursor (no rescan, so nothing is uploaded again).
			if !errors.Is(err, io.EOF) {
				r.stats.drop("zst_error")
				return nil
			}
			rc.Close()
			nrc, err := openContent(f)
			if err != nil {
				rc = nil
				return nil
			}
			rc = nrc
			start, ord, fs.scanGen, rd = 0, 0, cur.ScanGen+1, rc
		}
	}
	fs.ackEnd, fs.ackOrd = start, ord
	br := bufio.NewReaderSize(rd, 1<<20)
	if _, err := br.Peek(1); err != nil {
		if !errors.Is(err, io.EOF) {
			r.stats.drop("zst_error")
			return nil
		}
		// Nothing past the cursor (Codex compressed a rollout already read): record the new stat only,
		// without the header and coverage passes.
		fs.complete = true
		return r.fileDone(fs, 0)
	}
	cf, err := r.codexInfo(f)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // restored by Codex since it was opened: the plain file is read next run
		}
		return err
	}
	return r.readLines(fs, cf, br, start, ord, bt)
}

// readLines reads the complete lines of fs from rd, which is positioned at off (line ordinal ord).
func (r *runner) readLines(fs *fileState, cf *codexFile, rd *bufio.Reader, off, ord int64, bt *batch) error {
	f := fs.ref
	units := 0
	for {
		if r.opts.maxBytes > 0 && r.stats.bytesRead >= r.opts.maxBytes {
			r.limitHit = true
			break
		}
		line, err := rd.ReadBytes('\n')
		if err != nil {
			// At EOF a trailing partial line waits for its newline. Any other error (a damaged compressed
			// rollout) leaves the file incomplete, so its stat is not recorded and the next run reads it again.
			fs.complete = errors.Is(err, io.EOF)
			if !fs.complete {
				r.stats.drop("read_error")
			}
			break
		}
		ord++
		u := &unitRec{fs: fs, ordinal: ord, offset: off, length: int64(len(line) - 1), end: off + int64(len(line))}
		off += int64(len(line))
		r.stats.bytesRead += int64(len(line))
		r.stats.lines++
		content := bytes.TrimSuffix(line[:len(line)-1], []byte{'\r'})
		if len(bytes.TrimSpace(content)) > 0 {
			t0 := time.Now()
			var bl *built
			if cf != nil {
				bl, err = r.b.codexRecord(f, cf, content, ord)
			} else {
				bl, err = r.b.claudeRecord(f, content)
			}
			r.stats.took(time.Since(t0))
			switch {
			case errors.Is(err, errSkip):
				if !bytes.HasPrefix(bytes.TrimSpace(content), []byte("{")) {
					r.stats.drop("parse_error")
				}
			case err != nil:
				return err
			default:
				u.b = bl
				r.countRecord(bl)
			}
		}
		units++
		if err := r.add(bt, u); err != nil {
			return err
		}
	}
	return r.fileDone(fs, units)
}

// processWhole re-reads a whole file (import.file) when its stat changed.
func (r *runner) processWhole(fs *fileState, bt *batch) error {
	t0 := time.Now()
	bl, n, err := r.b.readWhole(fs.ref, fs.fh, r.embeddedScripts)
	r.stats.took(time.Since(t0))
	if errors.Is(err, errRead) {
		return nil
	}
	if fs.has {
		fs.scanGen = fs.cur.ScanGen + 1
	}
	r.stats.bytesRead += n
	u := &unitRec{fs: fs, length: n, end: n}
	switch {
	case errors.Is(err, errSkip):
	case err != nil:
		return err
	default:
		u.b = bl
		r.stats.records++
	}
	fs.complete = true
	if err := r.add(bt, u); err != nil {
		return err
	}
	return r.fileDone(fs, 1)
}

// fileDone records a file that produced no unit (only its stat changed) at once; files with units are
// recorded when their batch is acknowledged.
func (r *runner) fileDone(fs *fileState, units int) error {
	if units > 0 && fs.pending > 0 {
		return nil
	}
	if r.opts.dryRun {
		return nil
	}
	c := r.cursorFor(fs, fs.complete)
	return r.st.tx(func(t *sql.Tx) error {
		r.cursors[c.FileKey] = c
		return putCursor(t, c)
	})
}

// cursorFor is the cursor of fs at its acknowledged position. The stat (statOK) is recorded only once
// the file was read to its end with every unit acknowledged, so a file cut off by --max-bytes or a
// failed upload is read again next run (mtime 0 never matches).
func (r *runner) cursorFor(fs *fileState, statOK bool) Cursor {
	f := fs.ref
	mtime := int64(0)
	if statOK {
		mtime = f.MtimeNS
	}
	obx := fs.ackObx
	if obx == "" {
		obx = r.meta.OutboxID
	}
	if int64(len(fs.prefix)) < min(fs.ackEnd, headBytes) {
		// The file grew past the prefix read at open while it was being read: read it again.
		fs.prefix = contentPrefix(f, headBytes)
	}
	return Cursor{
		FileKey: f.Key, Inode: f.Inode, Size: f.Size, MtimeNS: mtime, EndOffset: fs.ackEnd, LineOrdinal: fs.ackOrd,
		HeadSHA: headOf(fs.prefix, fs.ackEnd), ScanGen: fs.scanGen, LastSeq: fs.ackSeq, OutboxID: obx,
	}
}

// readPrefix reads up to n bytes from the start of fh.
func readPrefix(fh *os.File, n int64) []byte {
	buf := make([]byte, n)
	k, _ := fh.ReadAt(buf, 0)
	return buf[:k]
}

// headOf is head_sha256 for a cursor at end: sha256 of the file's first min(end, 4 KiB) bytes, so a
// file that only grew keeps its head while a rewrite of consumed bytes changes it. "" when the prefix is
// shorter than that (never equal to a stored head).
func headOf(prefix []byte, end int64) string {
	n := min(end, headBytes)
	if int64(len(prefix)) < n {
		return ""
	}
	sum := sha256.Sum256(prefix[:n])
	return hex.EncodeToString(sum[:])
}

// codexInfo reads a rollout's header and, for a session that went through CPA (model_provider
// cliproxyapi) with responses CPA holds in full, the per-call grouping that decides which records go
// metadata-only. Every other rollout uploads full, and so does every record of a dry run.
func (r *runner) codexInfo(f fileRef) (*codexFile, error) {
	cf, err := readCodexHeader(f)
	if err != nil {
		return nil, err
	}
	if cf.provider != "cliproxyapi" || r.opts.dryRun || cf.sessionID == "" && cf.threadID == "" {
		return cf, nil
	}
	covered, err := r.coverageFor(cf)
	if err != nil {
		return nil, err
	}
	if len(covered) == 0 {
		return cf, nil
	}
	cf.covered = covered
	if cf.groups, err = codexGroups(f); err != nil {
		return nil, err
	}
	return cf, nil
}

// coverageFor asks session-coverage for codex:<session_id> and codex:<thread_id>: the response ids CPA
// holds with content.
func (r *runner) coverageFor(cf *codexFile) (map[string]bool, error) {
	var ids []string
	for _, id := range []string{cf.sessionID, cf.threadID} {
		if id != "" && (len(ids) == 0 || ids[0] != "codex:"+id) {
			ids = append(ids, "codex:"+id)
		}
	}
	out := map[string]bool{}
	var ask []string
	for _, id := range ids {
		if c, ok := r.coverage[id]; ok {
			for k := range c {
				out[k] = true
			}
		} else {
			ask = append(ask, id)
		}
	}
	if len(ask) == 0 {
		return out, nil
	}
	got, err := r.cl.SessionCoverage(r.ctx, ask)
	if err != nil {
		return nil, r.requestError(err)
	}
	if r.mismatch {
		return nil, errEpoch
	}
	for _, id := range ask {
		c := map[string]bool{}
		for _, rid := range got[id].Responses {
			c[rid] = true
			out[rid] = true
		}
		r.coverage[id] = c
	}
	return out, nil
}

// countRecord counts one record a run or requeue will send or dead-letter, and as fitted when the fit
// moved frame subtrees of it into pieces (the summary's fitted=N).
func (r *runner) countRecord(bl *built) {
	r.stats.records++
	if bl.fitFrom != "" {
		r.stats.status("fitted")
	}
}

// add appends a unit and flushes the batch at the producer targets (500 envelopes, about 8 MiB), or
// once the pieces it holds reach maxHeldBytes. A too-large unit holds no pieces (markTooLarge).
func (r *runner) add(bt *batch, u *unitRec) error {
	bt.units = append(bt.units, u)
	u.fs.pending++
	if u.b != nil && !u.b.tooLarge {
		bt.nEnv++
		bt.bytes += int64(len(u.b.body)) + 1024 + wire.CAB1RecordOverhead
		for _, p := range u.b.pieces {
			if bt.pieces[p.Hash] {
				continue
			}
			bt.pieces[p.Hash] = true
			bt.held += int64(len(p.Bytes))
			if int64(len(p.Bytes)) <= r.limits.PieceInline {
				bt.bytes += int64(len(p.Bytes)) + wire.CAB1RecordOverhead
			}
		}
	}
	maxEnv := min(wire.TargetCommitEnvelopes, r.limits.CommitEnvelopes)
	if bt.nEnv >= maxEnv || bt.bytes >= wire.TargetCommitBytes || bt.held >= maxHeldBytes || len(bt.units) >= maxBatchUnits {
		if err := r.flush(bt); err != nil {
			return err
		}
	}
	r.beat()
	return nil
}

// beat sends a heartbeat with state partial once runHeartbeatEvery has passed since the run started
// or since its last heartbeat, so a long run keeps the archive's dead-man quiet. A failed send never
// stops the run; it only records the error class, as the run's last heartbeat does.
func (r *runner) beat() {
	if r.opts.dryRun || r.b == nil || r.meta.OutboxID == "" {
		return
	}
	if now := r.env.now(); now.Sub(r.lastHB) >= runHeartbeatEvery {
		r.sendHeartbeat("partial", r.faultList())
	}
}

// sendHeartbeat sends one heartbeat of this run.
func (r *runner) sendHeartbeat(state string, faults []string) {
	r.hbN++
	r.lastHB = r.env.now()
	if _, err := r.cl.Heartbeat(r.ctx, r.heartbeat(state, faults)); err != nil {
		_ = r.requestError(err)
	}
}

// faultList returns the run's fault names, sorted.
func (r *runner) faultList() []string {
	faults := make([]string, 0, len(r.faults))
	for f := range r.faults {
		faults = append(faults, f)
	}
	sort.Strings(faults)
	return faults
}

// flush assigns seqs, uploads the batch and, in one transaction after the acknowledgement, advances
// cursors and next_seq and records dead letters and tombstones.
func (r *runner) flush(bt *batch) error {
	if len(bt.units) == 0 {
		return nil
	}
	defer func() {
		for _, u := range bt.units {
			u.fs.pending--
		}
		bt.units, bt.nEnv, bt.bytes, bt.held, bt.pieces = nil, 0, 0, 0, map[wire.Hash]bool{}
	}()
	next := r.meta.NextSeq
	var send []*unitRec
	for _, u := range bt.units {
		if u.b == nil {
			u.done = true
			continue
		}
		u.seq = next
		next++
		if u.b.tooLarge {
			r.localDead(u, wire.TombRejectedTooLarge, wire.TombRejectedTooLarge, nil)
			continue
		}
		raw, err := r.envelope(u)
		if err != nil {
			return err
		}
		u.raw = raw
		send = append(send, u)
		r.stats.envelopes++
	}
	if r.opts.dryRun {
		for _, u := range send {
			u.done, u.status = true, "dry-run"
		}
		r.meta.NextSeq = next
		return nil
	}
	upErr := r.upload(send)
	if r.env.hooks != nil && r.env.hooks.afterCommit != nil {
		if err := r.env.hooks.afterCommit(); err != nil {
			return err // simulated crash between the commit and the cursor update
		}
	}
	if err := r.persist(bt.units); err != nil {
		return err
	}
	if upErr != nil {
		return upErr
	}
	for _, u := range bt.units {
		if !u.done {
			r.fault("incomplete")
			return errIncomplete
		}
	}
	return nil
}

// envelope encodes u's envelope with its seq and locator.
func (r *runner) envelope(u *unitRec) ([]byte, error) {
	f := u.fs.ref
	loc := &wire.Locator{MachineID: r.cfg.MachineID, FileKey: f.Key, ScanGen: u.fs.scanGen}
	if f.Whole {
		mt := mtimeOf(f)
		loc.Size, loc.ModTime = u.length, &mt
	} else {
		loc.LineOrdinal, loc.ByteOffset, loc.ByteLen, loc.EndOffset = u.ordinal, u.offset, u.length, u.end
	}
	return canon.Marshal(wire.Envelope{
		V: wire.ProtocolV1, Kind: u.b.kind, OutboxID: r.meta.OutboxID, Seq: u.seq, EpochSeen: r.meta.EpochSeen,
		Key: u.b.key, MaskVersion: mask.Version, CanonVersion: canon.Version, ExactRev: r.b.m.ExactRev(),
		CreatedAt: u.b.createdAt, Refs: u.b.refs, Loc: loc, Body: u.b.body,
	})
}

// localDead dead-letters u locally: it never reaches the server as an envelope, and a tombstone for its
// seq goes in the next commit, so the archive's hw can pass it.
func (r *runner) localDead(u *unitRec, deadReason, tombReason string, rules []string) {
	u.done, u.deadReason, u.tombReason, u.rules = true, deadReason, tombReason, rules
	r.addTomb(TombRow{Seq: u.seq, Key: u.b.key, Reason: tombReason})
	r.stats.status("local:" + tombReason)
}

// addTomb queues a new tombstone, replacing one already queued for the same seq, so r.tombs holds at
// most one tombstone per seq (the tombstones table's primary key).
func (r *runner) addTomb(tb TombRow) {
	for _, t := range r.tombs {
		if t.Seq == tb.Seq {
			t.TombRow, t.acked, t.isNew = tb, false, true
			return
		}
	}
	r.tombs = append(r.tombs, &tombState{TombRow: tb, isNew: true})
}

// upload commits units, re-sending seqs answered retry for up to maxRounds rounds.
func (r *runner) upload(units []*unitRec) error {
	pending := units
	for round := 1; ; round++ {
		retry, err := r.send(pending)
		if err != nil {
			return err
		}
		if len(retry) == 0 || round >= maxRounds {
			return nil
		}
		busy := false
		for _, u := range retry {
			if u.reason != wire.ReasonMissingRef {
				busy = true
			}
		}
		if busy {
			if err := r.sleep(r.backoff(round)); err != nil {
				return err
			}
		}
		pending = retry
	}
}

// send applies the whole-request rules around one commit: auth backoff, epoch change, halving on
// 400 and 413, and backoff on 429, 503, 5xx and network errors.
func (r *runner) send(units []*unitRec) ([]*unitRec, error) {
	fails := 0
	for {
		retry, err := r.commitOnce(units)
		if err == nil {
			r.failures, r.lastErr, r.lastAck = 0, "none", r.env.now()
			return retry, nil
		}
		var tooLarge bool
		switch {
		case errors.Is(err, errEpoch):
			r.lastErr = "409"
			return nil, err
		case errors.Is(err, client.ErrMalformed):
			r.lastErr = "400"
		case errors.Is(err, client.ErrTooLarge):
			r.lastErr, tooLarge = "413", true
		default:
			if err := r.requestError(err); !errors.Is(err, errTransient) {
				return nil, err
			}
			fails++
			r.failures++
			if fails > maxTransient {
				r.fault("upload_failed")
				return nil, errTransient
			}
			wait := r.backoff(fails)
			var ra *client.RetryAfterError
			if errors.As(err, &ra) && ra.After > 0 {
				wait = min(ra.After, maxBackoff)
			}
			if err := r.sleep(wait); err != nil {
				return nil, err
			}
			continue
		}
		// 400 or 413: halve the batch; one envelope that still fails is dead-lettered locally.
		reason := wire.TombRejectedLocal
		if tooLarge {
			reason = wire.TombRejectedTooLarge
		}
		switch len(units) {
		case 0:
			return nil, fmt.Errorf("tombstone-only commit refused: %w", err)
		case 1:
			r.localDead(units[0], reason, reason, nil)
			return nil, nil
		}
		h := len(units) / 2
		ra, err := r.send(units[:h])
		if err != nil {
			return nil, err
		}
		rb, err := r.send(units[h:])
		if err != nil {
			return nil, err
		}
		return append(ra, rb...), nil
	}
}

// requestError classifies a whole-request error: 401/403 stop uploads for 15 minutes (never retried),
// unsupported_protocol keeps everything pending, 409 epoch_changed is the epoch rule, and the rest
// (429/503, 5xx, network) is transient.
func (r *runner) requestError(err error) error {
	var ec *client.EpochChangedError
	var se *client.StatusError
	switch {
	case errors.Is(err, client.ErrUnauthorized):
		r.lastErr = "401"
		r.fault("auth_failed")
		r.meta.AuthBackoffUntil = r.env.now().Add(authBackoff).Unix()
		if r.meta.OutboxID != "" && !r.opts.dryRun {
			_ = r.st.setAuthBackoff(r.meta.AuthBackoffUntil)
		}
		return errAuth
	case errors.Is(err, client.ErrUnsupportedProtocol):
		r.fault("server_too_old")
		return errStop
	case errors.As(err, &ec):
		r.lastErr = "409"
		return errEpoch
	case errors.Is(err, errEpoch):
		return err
	case errors.As(err, &se):
		r.lastErr = "5xx"
		if se.StatusCode < 500 {
			r.lastErr = fmt.Sprint(se.StatusCode)
		}
	default:
		var ra *client.RetryAfterError
		if errors.As(err, &ra) {
			r.lastErr = "429"
		} else {
			r.lastErr = "net"
		}
	}
	return fmt.Errorf("%w: %w", errTransient, err)
}

// commitOnce runs need, put and commit for units and every unacked tombstone and
// applies the per-seq results. It returns the units answered retry.
func (r *runner) commitOnce(units []*unitRec) ([]*unitRec, error) {
	pieces := map[wire.Hash]wire.BatchPiece{}
	var refs []wire.Hash
	for _, u := range units {
		if u.done {
			continue
		}
		for _, p := range u.b.pieces {
			pieces[p.Hash] = wire.BatchPiece{Hash: p.Hash, Kind: p.Kind, ContentType: p.ContentType, Raw: p.Bytes}
		}
		for _, h := range u.b.refs {
			if _, ok := pieces[h]; ok && !r.have[h] {
				refs = append(refs, h)
			}
		}
	}
	refs = uniqHashes(refs)
	need := map[wire.Hash]bool{}
	for i := 0; i < len(refs); i += r.limits.NeedHashes {
		chunk := refs[i:min(i+r.limits.NeedHashes, len(refs))]
		n, staged, _, err := r.cl.Need(r.ctx, chunk)
		if err != nil {
			return nil, err
		}
		if r.mismatch {
			return nil, errEpoch
		}
		for _, h := range n {
			need[h] = true
		}
		for _, h := range staged {
			delete(r.have, h)
		}
		for _, h := range chunk {
			if !need[h] && !containsHash(staged, h) {
				r.addHave(h)
			}
		}
	}
	var envBytes int64
	for _, u := range units {
		envBytes += int64(len(u.raw)) + wire.CAB1RecordOverhead
	}
	budget := r.limits.CommitRaw - envBytes - int64(len(r.tombs))*(512+wire.CAB1RecordOverhead) - (1 << 20)
	var inline []wire.BatchPiece
	var used int64
	for _, h := range refs {
		if !need[h] {
			continue
		}
		p := pieces[h]
		size := int64(len(p.Raw)) + 34 + int64(len(p.ContentType)) + wire.CAB1RecordOverhead
		if int64(len(p.Raw)) <= r.limits.PieceInline && used+size <= budget {
			inline = append(inline, p)
			used += size
			continue
		}
		if err := r.cl.PutPiece(r.ctx, h, p.Kind, p.ContentType, p.Raw); err != nil {
			var se *client.StatusError
			if errors.As(err, &se) && se.StatusCode == 422 {
				for _, u := range units {
					if !u.done && containsHash(u.b.refs, h) {
						r.localDead(u, string(wire.ReasonMaskViolation), wire.TombRejectedLocal, se.Rules)
					}
				}
				continue
			}
			return nil, err
		}
		if r.mismatch {
			return nil, errEpoch
		}
	}
	// One item per seq: results are matched to items by seq (the archive answers every seq exactly
	// once), so a tombstone never shares a commit with an envelope or another tombstone of
	// its seq; one that would waits for a later commit.
	var envs [][]byte
	var sent []*unitRec
	inCommit := map[uint64]bool{}
	for _, u := range units {
		if !u.done {
			envs = append(envs, u.raw)
			sent = append(sent, u)
			inCommit[u.seq] = true
		}
	}
	var tombs []wire.Tombstone
	var tsent []*tombState
	for _, t := range r.tombs {
		if !t.acked && !inCommit[t.Seq] {
			inCommit[t.Seq] = true
			tombs = append(tombs, wire.Tombstone{V: wire.ProtocolV1, OutboxID: r.meta.OutboxID, Seq: t.Seq, Key: t.Key, Reason: t.Reason})
			tsent = append(tsent, t)
		}
	}
	if len(envs) == 0 && len(tombs) == 0 {
		return nil, nil
	}
	if len(envs) == 0 {
		inline = nil
	}
	resp, err := r.cl.Commit(r.ctx, r.meta.EpochSeen, r.meta.ArchiveID, inline, envs, tombs)
	if err != nil {
		return nil, err
	}
	res := make(map[uint64]wire.CommitResult, len(resp.Results))
	for _, x := range resp.Results {
		res[x.Seq] = x
	}
	staged := map[wire.Hash]bool{}
	for _, h := range resp.Staged {
		staged[h] = true
		delete(r.have, h)
	}
	if hw, ok := resp.HW[r.meta.OutboxID]; ok {
		r.hw = hw
	}
	for _, t := range tsent {
		if x, ok := res[t.Seq]; ok && x.Status != wire.StatusRetry {
			t.acked = true
		}
	}
	var retry []*unitRec
	for _, u := range sent {
		x, ok := res[u.seq]
		if !ok {
			x = wire.CommitResult{Seq: u.seq, Status: wire.StatusRetry, Reason: wire.ReasonBusy}
		}
		u.status, u.reason = x.Status, x.Reason
		r.stats.status(string(x.Status))
		switch x.Status {
		case wire.StatusOK, wire.StatusDuplicate, wire.StatusConflict:
			u.done = true
			for _, h := range u.b.refs {
				if !staged[h] {
					r.addHave(h)
				}
			}
		case wire.StatusRejected:
			u.done, u.deadReason = true, string(x.Reason)
		default: // retry
			if x.Reason == wire.ReasonMissingRef {
				for _, h := range x.Missing {
					delete(r.have, h)
				}
				u.missing++
				if u.missing >= missingRefLimit {
					r.localDead(u, wire.TombMissingRef, wire.TombMissingRef, nil)
					continue
				}
			}
			retry = append(retry, u)
		}
	}
	if r.mismatch {
		return retry, errEpoch
	}
	return retry, nil
}

func (r *runner) addHave(h wire.Hash) {
	if len(r.have) >= haveCacheMax {
		r.have = map[wire.Hash]bool{}
	}
	r.have[h] = true
}

// persist writes, in one transaction, every file's cursor advanced over its acknowledged prefix, dead
// rows, tombstones and next_seq. A file's cursor stops before its first unit without a
// receipt; that unit and the ones after it are read again by the next run.
func (r *runner) persist(units []*unitRec) error {
	var order []*fileState
	seen := map[*fileState]bool{}
	blocked := map[*fileState]bool{}
	for _, u := range units {
		if u.requeue != nil {
			continue
		}
		if !seen[u.fs] {
			seen[u.fs] = true
			order = append(order, u.fs)
		}
		if blocked[u.fs] {
			continue
		}
		if !u.done {
			blocked[u.fs] = true
			continue
		}
		u.fs.ackEnd, u.fs.ackOrd = u.end, u.ordinal
		if u.seq > 0 {
			u.fs.ackSeq, u.fs.ackObx = u.seq, r.meta.OutboxID
		}
	}
	nextSeq := r.meta.NextSeq
	first := uint64(0)
	for _, u := range units {
		if u.seq == 0 {
			continue
		}
		if u.seq >= nextSeq {
			nextSeq = u.seq + 1
		}
		if !u.done && (first == 0 || u.seq < first) {
			first = u.seq
		}
	}
	// Seqs from first on are assigned again by the next run, so an unacked tombstone among them is stale:
	// sent later, it would share a commit and a seq with a new envelope, and its ok would stand for that
	// envelope's result. It is dropped; its dead row stays (requeue still finds it), and a record in a
	// blocked file is read again and gets a fresh tombstone.
	tombs := r.tombs
	if first > 0 {
		nextSeq = first
		tombs = nil
		for _, t := range r.tombs {
			if t.acked || t.Seq < first {
				tombs = append(tombs, t)
			}
		}
	}
	now := r.env.now().Unix()
	err := r.st.tx(func(t *sql.Tx) error {
		for _, fs := range order {
			c := r.cursorFor(fs, fs.complete && !blocked[fs])
			r.cursors[c.FileKey] = c
			if err := putCursor(t, c); err != nil {
				return err
			}
		}
		for _, u := range units {
			if !u.done || u.seq == 0 {
				continue
			}
			if u.deadReason != "" {
				d := r.deadRow(u, now)
				if err := putDead(t, d); err != nil {
					return err
				}
				r.stats.status("dead:" + u.deadReason)
			}
			if u.requeue != nil && (u.deadReason == "" || u.b.key != u.requeue.Key) {
				if err := setDeadState(t, u.requeue.Key, "requeued"); err != nil {
					return err
				}
			}
		}
		if err := persistTombs(t, tombs); err != nil {
			return err
		}
		if first > 0 {
			if _, err := t.Exec(`DELETE FROM tombstones WHERE seq >= ? AND state='pending'`, first); err != nil {
				return err
			}
		}
		m := r.meta
		m.NextSeq = nextSeq
		return putMeta(t, m)
	})
	if err == nil {
		r.meta.NextSeq = nextSeq // in memory only once the transaction committed
		r.tombs = tombs
	}
	return err
}

// persistTombs writes new tombstones and records acknowledged ones.
func persistTombs(t *sql.Tx, tombs []*tombState) error {
	for _, tb := range tombs {
		if tb.isNew {
			if err := addTombstone(t, tb.TombRow); err != nil {
				return err
			}
			tb.isNew = false
		}
		if tb.acked {
			if err := ackTombstone(t, tb.Seq); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) deadRow(u *unitRec, now int64) Dead {
	f := u.fs.ref
	d := Dead{
		Key: u.b.key, Kind: string(u.b.kind), FileKey: f.Key, ScanGen: u.fs.scanGen, LineOrdinal: u.ordinal,
		ByteOffset: u.offset, ByteLen: u.length, Reason: u.deadReason, RuleIDs: strings.Join(u.rules, ","),
		Seq: u.seq, DeadAt: now, State: "dead",
	}
	return d
}

// flushTombstones sends tombstones still unacked at the end of a sweep, so hw passes them in this run.
func (r *runner) flushTombstones() error {
	if r.opts.dryRun {
		return nil
	}
	unacked := false
	for _, t := range r.tombs {
		if !t.acked {
			unacked = true
		}
	}
	if !unacked {
		return nil
	}
	_, err := r.send(nil)
	if perr := r.st.tx(func(t *sql.Tx) error { return persistTombs(t, r.tombs) }); perr != nil {
		return perr
	}
	return err
}

func (r *runner) fault(name string) { r.faults[name] = true }

func (r *runner) backoff(n int) time.Duration {
	d := time.Second << min(n-1, 9)
	return time.Duration(rand.Int64N(int64(min(d, maxBackoff)) + 1)) // full jitter
}

func (r *runner) sleep(d time.Duration) error {
	if r.env.Sleep != nil {
		return r.env.Sleep(r.ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

// finish records the run in meta and sends the run's last heartbeat, as every importer run does at its
// end (a long run has also sent state partial heartbeats on the way, see beat). A run that hit 401
// sends none (it would only be refused).
func (r *runner) finish(runErr error) {
	if r.opts.dryRun {
		return
	}
	state := "ok"
	if runErr != nil || r.limitHit {
		state = "partial"
	}
	if runErr != nil && !errors.Is(runErr, errAuth) && !errors.Is(runErr, errTransient) && !errors.Is(runErr, errStop) &&
		!errors.Is(runErr, errEpoch) && !errors.Is(runErr, errIncomplete) {
		r.fault("local_error")
	}
	faults := r.faultList()
	if r.meta.OutboxID != "" {
		r.meta.LastRunAt, r.meta.LastRunState, r.meta.Faults = r.env.now().Unix(), state, strings.Join(faults, ",")
		_ = r.st.setRunInfo(r.meta.LastRunAt, state, r.meta.Faults)
	}
	if errors.Is(runErr, errAuth) || r.b == nil {
		return
	}
	r.sendHeartbeat(state, faults)
}

func (r *runner) heartbeat(state string, faults []string) wire.Heartbeat {
	dead, _ := r.st.count(`SELECT count(*) FROM dead WHERE state='dead'`)
	pend := 0
	for _, t := range r.tombs {
		if !t.acked {
			pend++
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p50, p99 := r.stats.percentiles()
	r.stats.mu.Lock()
	matches := make(map[string]uint64, len(r.stats.matches))
	for k, v := range r.stats.matches {
		matches[k] = v
	}
	dropped := make(map[string]uint64, len(r.stats.dropped))
	for k, v := range r.stats.dropped {
		dropped[k] = v
	}
	calls := map[string]uint64{"files": uint64(r.stats.files), "records": uint64(r.stats.records), "envelopes": uint64(r.stats.envelopes)}
	maskBytes := r.stats.maskBytes
	r.stats.mu.Unlock()
	now := r.env.now().UTC()
	return wire.Heartbeat{
		V: wire.ProtocolV1, OutboxID: r.meta.OutboxID, BootID: r.bootID, N: r.hbN, SentAt: now, StartedAt: r.started.UTC(),
		Component: "importer", Version: Version, State: state, MaskVersion: mask.Version, RulesetSHA: mask.RulesetSHA256(),
		ExactRev: r.b.m.ExactRev(), LastHookAt: map[string]time.Time{}, Calls: calls, HookErrors: map[string]uint64{},
		Panics: map[string]uint64{}, Dropped: dropped, HookP50us: map[string]uint32{}, HookP99us: map[string]uint32{},
		Outbox: wire.OutboxStats{Pending: uint64(pend), Dead: uint64(dead), NextSeq: r.meta.NextSeq, HW: r.hw, LastAckAt: r.lastAck},
		Uploader: wire.UploaderStats{
			LastSuccessAt: r.lastAck, LastErrorClass: r.lastErrClass(), ConsecutiveFailures: r.failures,
			EpochSeen: r.meta.EpochSeen, ArchiveID: r.meta.ArchiveID,
		},
		Runtime: wire.RuntimeStats{HeapInuse: ms.HeapInuse, Sys: ms.Sys, Goroutines: uint64(runtime.NumGoroutine()), GCCycles: uint64(ms.NumGC)},
		Masker: wire.MaskerStats{
			Bodies: uint64(r.stats.records), Bytes: uint64(maskBytes), MemoHits: r.b.memo.hits, MemoMisses: r.b.memo.misses,
			P50MS: p50, P99MS: p99, MatchesByRule: matches,
		},
		Importer: &wire.ImporterStats{LastScanAt: r.started.UTC(), FilesTracked: uint64(len(r.cursors)), BacklogBytes: r.backlog()},
		Faults:   append([]string{}, faults...),
	}
}

func (r *runner) lastErrClass() string {
	if r.lastErr == "" {
		return "none"
	}
	return r.lastErr
}

// backlog is the bytes of discovered files not yet behind a cursor.
func (r *runner) backlog() uint64 {
	var n int64
	for _, f := range r.files {
		c, ok := r.cursors[f.Key]
		switch {
		case !ok:
			n += f.Size
		case f.Whole, f.Zstd: // a compressed rollout counts its compressed size until read (an estimate)
			if c.Size != f.Size || c.MtimeNS != f.MtimeNS || f.Zstd && c.Inode != f.Inode {
				n += f.Size
			}
		case c.Inode != f.Inode && f.Role != RoleCodexRollout: // rescanned from 0 (processFile)
			n += f.Size
		case f.Size > c.EndOffset:
			n += f.Size - c.EndOffset
		}
	}
	return uint64(max(n, 0))
}

// summary prints the run's counts (never content).
func (r *runner) summary(w io.Writer, runErr error) {
	r.stats.mu.Lock()
	defer r.stats.mu.Unlock()
	keys := make([]string, 0, len(r.stats.statuses))
	for k := range r.stats.statuses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, r.stats.statuses[k]))
	}
	fmt.Fprintf(w, "files=%d lines=%d records=%d envelopes=%d bytes=%d %s\n",
		r.stats.files, r.stats.lines, r.stats.records, r.stats.envelopes, r.stats.bytesRead, strings.Join(parts, " "))
	if r.limitHit {
		fmt.Fprintln(w, "stopped at --max-bytes; the next run continues")
	}
	if runErr != nil {
		fmt.Fprintf(w, "run: %v\n", runErr)
	}
}

func uniqHashes(in []wire.Hash) []wire.Hash {
	seen := make(map[wire.Hash]bool, len(in))
	out := in[:0]
	for _, h := range in {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

func containsHash(hs []wire.Hash, h wire.Hash) bool {
	for _, x := range hs {
		if x == h {
			return true
		}
	}
	return false
}
