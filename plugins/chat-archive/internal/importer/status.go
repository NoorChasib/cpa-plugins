package importer

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// status prints cursors, backlog bytes, the last run, faults and dead letters by reason: counts, ids
// and times only. It does not take the lock: SQLite WAL lets it read beside a run.
func (e *Env) status() error {
	p := pathsFor(e.Home)
	cfg, cerr := loadConfig(p)
	if cerr != nil && !errors.Is(cerr, errNotEnrolled) {
		return cerr
	}
	if errors.Is(cerr, errNotEnrolled) {
		fmt.Fprintln(e.Stdout, "enrolled: no")
		cfg.Roots = DefaultRoots(e.Home)
	} else {
		fmt.Fprintf(e.Stdout, "enrolled: %s machine=%s label=%s\n", cfg.SourceID, cfg.MachineID, cfg.Label)
	}
	if _, err := os.Stat(p.StateDB); err != nil {
		fmt.Fprintln(e.Stdout, "state: none yet")
		return nil
	}
	st, err := openState(p.StateDB)
	if err != nil {
		return err
	}
	defer st.Close()
	m, err := st.meta()
	if err != nil && !errors.Is(err, errNoMeta) {
		return err
	}
	fmt.Fprintf(e.Stdout, "outbox: %s next_seq=%d epoch_seen=%d archive=%s\n", m.OutboxID, m.NextSeq, m.EpochSeen, m.ArchiveID)
	if m.LastRunAt > 0 {
		fmt.Fprintf(e.Stdout, "last run: %s state=%s\n", time.Unix(m.LastRunAt, 0).UTC().Format(time.RFC3339), m.LastRunState)
	} else {
		fmt.Fprintln(e.Stdout, "last run: never")
	}
	faults := m.Faults
	if faults == "" {
		faults = "none"
	}
	fmt.Fprintf(e.Stdout, "faults: %s\n", faults)
	if m.AuthBackoffUntil > e.now().Unix() {
		fmt.Fprintf(e.Stdout, "uploads paused after a 401 until %s\n", time.Unix(m.AuthBackoffUntil, 0).UTC().Format(time.RFC3339))
	}
	curs, err := st.cursors()
	if err != nil {
		return err
	}
	files, _ := discover(cfg.Roots)
	var backlog, pending int64
	for _, f := range files {
		c, ok := curs[f.Key]
		switch {
		case !ok:
			backlog += f.Size
			pending++
		case f.Whole && (c.Size != f.Size || c.MtimeNS != f.MtimeNS),
			f.Zstd && (c.Size != f.Size || c.MtimeNS != f.MtimeNS || c.Inode != f.Inode): // compressed size, an estimate
			backlog += f.Size
			pending++
		case f.Zstd:
		case !f.Whole && c.Inode != f.Inode && f.Role != RoleCodexRollout: // rescanned from 0 (processFile)
			backlog += f.Size
			pending++
		case !f.Whole && f.Size > c.EndOffset:
			backlog += f.Size - c.EndOffset
			pending++
		}
	}
	fmt.Fprintf(e.Stdout, "cursors: %d files tracked, %d of %d discovered files with new bytes, backlog %d bytes\n", len(curs), pending, len(files), backlog)
	tombs, _ := st.count(`SELECT count(*) FROM tombstones WHERE state='pending'`)
	fmt.Fprintf(e.Stdout, "tombstones pending: %d\n", tombs)
	by, err := st.deadByReason()
	if err != nil {
		return err
	}
	if len(by) == 0 {
		fmt.Fprintln(e.Stdout, "dead letters: none")
	}
	states := make([]string, 0, len(by))
	for s := range by {
		states = append(states, s)
	}
	sort.Strings(states)
	for _, s := range states {
		reasons := make([]string, 0, len(by[s]))
		for r := range by[s] {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		var parts []string
		for _, r := range reasons {
			parts = append(parts, fmt.Sprintf("%s=%d", r, by[s][r]))
		}
		fmt.Fprintf(e.Stdout, "dead letters (%s): %s\n", s, strings.Join(parts, " "))
	}
	return nil
}

// deadList prints dead letters: key, reason, rule ids, file key shape and time; never content.
func (e *Env) deadList(reason string) error {
	p := pathsFor(e.Home)
	if _, err := os.Stat(p.StateDB); err != nil {
		fmt.Fprintln(e.Stdout, "no dead letters")
		return nil
	}
	st, err := openState(p.StateDB)
	if err != nil {
		return err
	}
	defer st.Close()
	rows, err := st.deadRows(reason, "", 0)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "no dead letters")
	}
	for _, d := range rows {
		rules := d.RuleIDs
		if rules == "" {
			rules = "-"
		}
		fmt.Fprintf(e.Stdout, "%s %s reason=%s rules=%s state=%s file=%s scan_gen=%d line=%d seq=%d\n",
			time.Unix(d.DeadAt, 0).UTC().Format(time.RFC3339), d.Key, d.Reason, rules, d.State,
			fileKeyShape(d.FileKey), d.ScanGen, d.LineOrdinal, d.Seq)
	}
	return nil
}
