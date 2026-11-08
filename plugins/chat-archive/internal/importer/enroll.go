package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// enroll registers this machine's importer with a single-use code: machine_id is
// created on first enrollment and kept after; config.json and the 0600 token are written; the state
// gets a fresh outbox, because hw is per (source, outbox) and the new source starts at seq 1. When the
// archive id differs from the one the state last saw, every cursor is reset, so the new archive gets the
// whole history.
func (e *Env) enroll(archiveURL, code, label string, intermittent bool) error {
	p := pathsFor(e.Home)
	release, _, err := acquireLock(p.LockFile, true)
	if err != nil {
		return err
	}
	defer release()
	prev, err := loadConfig(p)
	if err != nil && !errors.Is(err, errNotEnrolled) {
		return err
	}
	host, _ := os.Hostname()
	if label == "" {
		label = host
	}
	machineID := prev.MachineID
	if machineID == "" {
		machineID = newUUID()
	}
	resp, err := client.Enroll(context.Background(), strings.TrimRight(archiveURL, "/"), wire.EnrollRequest{
		Code: code, Kind: "importer", MachineID: machineID, Hostname: host, Label: label,
		OS: runtime.GOOS, Arch: runtime.GOARCH, ClientVersion: Version, Intermittent: intermittent,
	})
	if err != nil {
		if errors.Is(err, client.ErrUnauthorized) {
			return errors.New("the archive refused the code (expired, used or wrong); get a new one")
		}
		return err
	}
	roots := prev.Roots
	if len(roots) == 0 {
		roots = DefaultRoots(e.Home)
	}
	cfg := Config{ArchiveURL: strings.TrimRight(archiveURL, "/"), SourceID: resp.SourceID, MachineID: machineID, Label: label, Roots: roots}
	if err := saveToken(p, resp.Token); err != nil {
		return err
	}
	if err := saveConfig(p, cfg); err != nil {
		return err
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
	prevArchive := m.ArchiveID
	m.OutboxID, m.NextSeq, m.EpochSeen, m.ArchiveID, m.AuthBackoffUntil = randomHex(16), 1, resp.Epoch, resp.ArchiveID, 0
	var reset int64
	if err := st.tx(func(t *sql.Tx) error {
		if _, err := t.Exec(`DELETE FROM tombstones`); err != nil {
			return err
		}
		if prevArchive != "" && prevArchive != resp.ArchiveID {
			// Another archive holds none of what the cursors passed: every file is read again from offset 0
			// (a changed archive id is a different archive), as epochReset does.
			res, err := t.Exec(`UPDATE cursors SET size=-1, end_offset=0, line_ordinal=0, head_sha256=''`)
			if err != nil {
				return err
			}
			if reset, err = res.RowsAffected(); err != nil {
				return err
			}
		}
		return putMeta(t, m)
	}); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "enrolled as %s (machine %s)\n", resp.SourceID, machineID)
	if reset > 0 {
		fmt.Fprintf(e.Stdout, "new archive: %d files to read again from the start\n", reset)
	}
	return nil
}

// registerCaller names the caller fingerprint of a CPA client key: CallerScope is computed
// locally and only the fingerprint is sent, once, with the source token.
func (e *Env) registerCaller(keyFile, name string) error {
	p := pathsFor(e.Home)
	cfg, err := loadConfig(p)
	if err != nil {
		return err
	}
	tok, err := loadToken(p)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	fp := fingerprint.CallerScope(string(b))
	if fp == "" {
		return errors.New("the key file is empty")
	}
	c := client.New(cfg.ArchiveURL, tok, client.Options{HTTPClient: e.HTTPClient, UserAgent: "chat-archive-import/" + Version})
	if err := c.PutCaller(context.Background(), fp, wire.CallerRequest{Machine: name, App: "chat-archive-import"}); err != nil {
		var se *client.StatusError
		if errors.As(err, &se) && se.StatusCode == 409 {
			return fmt.Errorf("caller %s… is already named differently", fp[:12])
		}
		return err
	}
	fmt.Fprintf(e.Stdout, "caller %s… named %s\n", fp[:12], name)
	return nil
}
