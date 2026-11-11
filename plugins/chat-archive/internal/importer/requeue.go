package importer

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

type requeueOpts struct {
	reason string
	since  int64
	remask bool
	split  bool
	dryRun bool
}

// requeueCmd re-reads each dead record from its source file at the recorded position, rebuilds it with
// the current masker (--remask) or with large frame strings as pieces against the current limits
// (--split), and uploads it with a new seq; the dead row becomes requeued. A record whose source
// bytes changed or vanished is gone. Without --remask or --split the rebuilt key must equal
// the dead key, which proves the bytes are unchanged; with them the key may differ, and the position
// checks (same scan_gen, the bytes still one complete line) decide.
func (e *Env) requeueCmd(o requeueOpts) int {
	p := pathsFor(e.Home)
	release, _, err := acquireLock(p.LockFile, true)
	if err != nil {
		fmt.Fprintf(e.Stderr, "chat-archive-import: lock: %v\n", err)
		return 1
	}
	defer release()
	r, err := e.newRunner(runOpts{dryRun: o.dryRun})
	if err != nil {
		fmt.Fprintf(e.Stderr, "chat-archive-import: %v\n", err)
		return 1
	}
	defer r.close()
	runErr := r.requeue(o)
	r.finish(runErr)
	fmt.Fprintf(e.Stdout, "requeue %s: found=%d gone=%d\n", o.reason, r.stats.records, r.requeueGone)
	r.summary(e.Stdout, runErr)
	if runErr != nil {
		return 1
	}
	return 0
}

func (r *runner) requeue(o requeueOpts) error {
	if !r.opts.dryRun && r.meta.AuthBackoffUntil > r.env.now().Unix() {
		return errAuth
	}
	if err := r.start(); err != nil {
		return err
	}
	r.b.split = o.split
	rows, err := r.st.deadRows(o.reason, "dead", o.since)
	if err != nil {
		return err
	}
	bt := &batch{pieces: map[wire.Hash]bool{}}
	var gone []string
	for _, d := range rows {
		u, err := r.rebuildDead(d, o)
		if err != nil {
			return err
		}
		if u == nil {
			gone = append(gone, d.Key)
			continue
		}
		r.stats.records++
		if r.opts.dryRun {
			continue
		}
		if err := r.add(bt, u); err != nil {
			return err
		}
	}
	r.requeueGone = len(gone)
	if !r.opts.dryRun && len(gone) > 0 {
		if err := r.st.tx(func(t *sql.Tx) error {
			for _, k := range gone {
				if err := setDeadState(t, k, "gone"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := r.flush(bt); err != nil {
		return err
	}
	return r.flushTombstones()
}

// rebuildDead rebuilds one dead row's envelope from its source, or returns nil when it is gone.
func (r *runner) rebuildDead(d Dead, o requeueOpts) (*unitRec, error) {
	root, rel, _ := strings.Cut(d.FileKey, "/")
	f, ok := classify(root, rel)
	dir := r.cfg.Roots[root]
	if !ok || dir == "" {
		return nil, nil
	}
	f.Path = filepath.Join(dir, filepath.FromSlash(rel))
	if err := statRef(&f); err != nil {
		if f.Role != RoleCodexRollout {
			return nil, nil
		}
		// Codex may have compressed the rollout since: its content is the same file's (zst.go).
		f.Path, f.Zstd = f.Path+zstSuffix, true
		if err := statRef(&f); err != nil {
			return nil, nil
		}
	}
	if c, ok := r.cursors[d.FileKey]; !ok || c.ScanGen != d.ScanGen {
		return nil, nil // rescanned since: the recorded position no longer names these bytes
	}
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil, nil
	}
	defer fh.Close() // the rebuilt unit needs no file afterwards: requeue never moves a cursor
	fs := &fileState{ref: f, scanGen: d.ScanGen}
	u := &unitRec{fs: fs, ordinal: d.LineOrdinal, offset: d.ByteOffset, length: d.ByteLen, requeue: &d}
	var bl *built
	if f.Whole {
		var n int64
		bl, n, err = r.b.readWhole(f, fh, r.embeddedScripts)
		if errors.Is(err, errRead) || n != d.ByteLen {
			return nil, nil
		}
		u.end = d.ByteLen
		if err != nil && !errors.Is(err, errSkip) {
			return nil, err
		}
	} else {
		// The record's line and the byte before it (the previous line's newline), from the content: the file
		// itself, or a compressed rollout's decompressed stream.
		lo := max(d.ByteOffset-1, 0)
		buf := make([]byte, d.ByteOffset-lo+d.ByteLen+1)
		if err := readContentAt(f, buf, lo); err != nil || buf[len(buf)-1] != '\n' || d.ByteOffset > 0 && buf[0] != '\n' {
			return nil, nil
		}
		buf = buf[d.ByteOffset-lo:]
		u.end = d.ByteOffset + d.ByteLen + 1
		content := buf[:d.ByteLen]
		if len(content) > 0 && content[len(content)-1] == '\r' {
			content = content[:len(content)-1]
		}
		if f.Role == RoleCodexRollout {
			cf, cerr := r.codexInfo(f)
			if cerr != nil {
				return nil, cerr
			}
			bl, err = r.b.codexRecord(f, cf, content, d.LineOrdinal)
		} else {
			bl, err = r.b.claudeRecord(f, content)
		}
		if err != nil && !errors.Is(err, errSkip) {
			return nil, err
		}
	}
	if bl == nil || !o.remask && !o.split && bl.key != d.Key {
		return nil, nil
	}
	u.b = bl
	return u, nil
}
