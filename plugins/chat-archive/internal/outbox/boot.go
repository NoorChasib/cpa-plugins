package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// BootCounters is one plugin boot's dropped.* counters as last persisted. The archive derives drop
// deltas from heartbeats only, per (source, boot_id), and a boot's counters live in memory: a CPA crash
// between heartbeats would lose the drops counted since the last one, and a graceful stop loses those
// counted after its final heartbeat (work abandoned while capture drains). The uploader saves them here
// whenever they change; the next boot that opens this outbox reports each other boot's record in a
// heartbeat under that boot_id, then deletes it. The archive takes a boot's last heartbeat as its total,
// so reporting the same counters twice adds nothing.
type BootCounters struct {
	BootID    string            `json:"boot_id"`
	StartedAt time.Time         `json:"started_at"`
	Component string            `json:"component"`
	Version   string            `json:"version"`
	N         uint64            `json:"n"` // the boot's last heartbeat n when saved
	Dropped   map[string]uint64 `json:"dropped"`
}

// MaxBootRecords bounds the boot records kept: past it the oldest saved are deleted (a crash loop while
// the archive is unreachable would otherwise add one per boot).
const MaxBootRecords = 32

// bootKeyPrefix prefixes each record's metadata key: no other key starts with it.
const bootKeyPrefix = "boot_counters/"

// ErrBootID is returned for a boot id that is empty, longer than 48 characters or not lowercase hex.
var ErrBootID = errors.New("outbox: invalid boot id")

func validBootID(id string) bool {
	if id == "" || len(id) > 48 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// bootKeys matches the record keys; the prefix holds '_', a LIKE wildcard, so it is escaped.
const bootKeys = `key LIKE 'boot\_counters/%' ESCAPE '\'`

// SaveBootCounters stores b, replacing the record of the same boot, in one durable transaction
// (synchronous=FULL). Records past MaxBootRecords are deleted oldest first.
func (o *Outbox) SaveBootCounters(ctx context.Context, b BootCounters) error {
	if o.closed.Load() {
		return ErrClosed
	}
	if !validBootID(b.BootID) {
		return ErrBootID
	}
	v, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = put(ctx, tx, bootKeyPrefix+b.BootID, string(v)); err != nil {
		return err
	}
	// An upsert keeps the row's rowid and a new key gets max(rowid)+1, so rowid orders records by
	// first save.
	if _, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE `+bootKeys+` AND rowid NOT IN
		(SELECT rowid FROM metadata WHERE `+bootKeys+` ORDER BY rowid DESC LIMIT ?)`, MaxBootRecords); err != nil {
		return err
	}
	return tx.Commit()
}

// BootCounters returns every saved record, oldest first. A record that does not decode is deleted.
func (o *Outbox) BootCounters(ctx context.Context) ([]BootCounters, error) {
	if o.closed.Load() {
		return nil, ErrClosed
	}
	rows, err := o.db.QueryContext(ctx, `SELECT key, value FROM metadata WHERE `+bootKeys+` ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var out []BootCounters
	var bad []string
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		var b BootCounters
		if json.Unmarshal([]byte(v), &b) != nil || bootKeyPrefix+b.BootID != k || !validBootID(b.BootID) {
			bad = append(bad, k)
			continue
		}
		out = append(out, b)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range bad {
		if _, err = o.db.ExecContext(ctx, "DELETE FROM metadata WHERE key=?", k); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteBootCounters deletes the record of bootID once a heartbeat reported it. A missing record is
// not an error.
func (o *Outbox) DeleteBootCounters(ctx context.Context, bootID string) error {
	if o.closed.Load() {
		return ErrClosed
	}
	if !validBootID(bootID) {
		return ErrBootID
	}
	_, err := o.db.ExecContext(ctx, "DELETE FROM metadata WHERE key=?", bootKeyPrefix+bootID)
	return err
}
