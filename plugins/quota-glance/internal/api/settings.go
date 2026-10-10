package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/aggregate"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/overrides"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

const (
	// settingsHeader carries the batch on the GET door.
	settingsHeader = "X-Quota-Glance-Settings"
	// maxSettingsBytes bounds the batch as JSON, on either door. Sixteen rows
	// are about 190 bytes each.
	maxSettingsBytes = 4096
)

// The doors, as the log names them.
const (
	DoorConsole  = "console"
	DoorPassword = "password"
)

// Saver is what the settings doors write through: the settings store, wrapped
// by the runtime so that a committed save is rebuilt into the document before
// it is answered. It is an interface for the same reason Redeemer is: nil
// closes the doors, and a test can see what was and was not saved.
type Saver interface {
	// Current is the settings in force.
	Current() overrides.Values
	// Save applies one batch, all or nothing, as overrides.Store.Apply does.
	// When it commits, it rebuilds and publishes the document before it
	// returns, and it never holds the store's lock while doing so.
	Save(batch overrides.Batch, now time.Time) (overrides.Result, error)
}

// SetSaver installs, or removes, the ability to change settings from the
// dashboard. Nil closes both doors: they 404 exactly as an unknown path does,
// so allow-edit off is indistinguishable from a build without the feature.
func (a *API) SetSaver(s Saver) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.saver = s
}

// settingsResponse is the console door: a POST under CPA's management tree,
// whose middleware has required the management key, in a header, before this
// runs. It copies the redeem POST's checks, in the redeem POST's order.
func (a *API) settingsResponse(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	saver, refusal := a.settingsSaver()
	if saver == nil {
		return refusal
	}
	// JSON only, for the reason the redeem POST gives: a form-encoded body is
	// the one a cross-site form can send without asking.
	if media := req.Headers.Get("Content-Type"); media != "" {
		if base, _, _ := strings.Cut(media, ";"); !strings.EqualFold(strings.TrimSpace(base), "application/json") {
			return jsonResponse(http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type"})
		}
	}
	if len(req.Body) > maxSettingsBytes {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	return a.save(saver, req.Body, DoorConsole, now)
}

// saveSettingsResponse is the password door: a GET on the resource tree, for
// a reader signed in with the web token, because a GET is all CPA dispatches
// there. It is fenced exactly as spendResponse is: nothing selecting the
// change is in the URL, a browser that says this is not a script on this page
// is refused before the token is looked at, early data is refused, and the
// token is checked against the shared limiter. A resent GET is harmless
// without a press id: a batch already applied is answered unchanged, and one
// another save overtook is a conflict, never a second change.
func (a *API) saveSettingsResponse(req protocol.ManagementRequest, now time.Time) protocol.ManagementResponse {
	if crossSite(req.Headers) {
		return jsonResponse(http.StatusForbidden, map[string]string{"error": "cross_site"})
	}
	if len(req.Headers.Values("Early-Data")) > 0 {
		return jsonResponse(http.StatusTooEarly, map[string]string{"error": "too_early"})
	}
	if !a.authorized(req.Headers) {
		return a.tokenRefusal(now)
	}
	saver, refusal := a.settingsSaver()
	if saver == nil {
		return refusal
	}
	values := req.Headers.Values(settingsHeader)
	if len(values) != 1 || len(values[0]) > base64.RawURLEncoding.EncodedLen(maxSettingsBytes) {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	raw, err := base64.RawURLEncoding.DecodeString(values[0])
	if err != nil || len(raw) > maxSettingsBytes {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	return a.save(saver, raw, DoorPassword, now)
}

// settingsSaver is the installed saver, or the answer when there is none to
// use: 404 with editing off, as with no redeemer, and 503 when settings.json
// could not be read.
func (a *API) settingsSaver() (Saver, protocol.ManagementResponse) {
	a.mu.RLock()
	saver := a.saver
	a.mu.RUnlock()
	if saver == nil {
		return nil, jsonResponse(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
	if saver.Current().Unreadable {
		return nil, jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "settings_unavailable"})
	}
	return saver, protocol.ManagementResponse{}
}

// save carries one batch from either door: its shape, its values, whether the
// served document offers every row it names, and then the store, which takes
// it all or nothing.
func (a *API) save(saver Saver, raw []byte, door string, now time.Time) protocol.ManagementResponse {
	batch, err := overrides.ParseBatch(raw)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	batch.Door = door
	doc := a.Document()
	fillFromDocument(&batch, doc)
	if fail := batch.Check(saver.Current(), now); fail != nil {
		return fieldRefusal(fail)
	}
	if refused := notEditable(batch, doc); len(refused) > 0 {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "not_editable", "ids": refused})
	}
	result, err := saver.Save(batch, now)
	var conflict *overrides.ConflictError
	var field *overrides.FieldError
	var refused *overrides.NotEditableError
	switch {
	case errors.As(err, &conflict):
		return jsonResponse(http.StatusConflict, map[string]any{
			"error": "conflict", "revision": conflict.Revision, "conflicts": conflict.IDs, "current": currentBlocks(conflict, doc),
		})
	case errors.As(err, &field):
		return fieldRefusal(field)
	case errors.As(err, &refused):
		return jsonResponse(http.StatusConflict, map[string]any{"error": "not_editable", "ids": refused.IDs})
	case errors.Is(err, overrides.ErrTooManyWrites):
		res := jsonResponse(http.StatusTooManyRequests, map[string]string{"error": "too_many_writes"})
		res.Headers.Set("Retry-After", "60")
		return res
	case errors.Is(err, overrides.ErrFull):
		return jsonResponse(http.StatusConflict, map[string]string{"error": "settings_full"})
	case errors.Is(err, overrides.ErrUnavailable):
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "settings_unavailable"})
	case err != nil:
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "settings_unwritable"})
	}
	revision := strconv.FormatUint(result.Revision, 10)
	if result.Unchanged {
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "unchanged": true, "revision": revision})
	}
	// The saver rebuilt and published before returning, so the document now
	// served is the one the page will read next, and its blocks are what the
	// rows now say.
	return jsonResponse(http.StatusOK, map[string]any{
		"ok": true, "unchanged": false, "revision": revision, "settings": savedBlocks(batch, a.Document()),
	})
}

func fieldRefusal(fail *overrides.FieldError) protocol.ManagementResponse {
	return jsonResponse(http.StatusBadRequest, map[string]string{"error": fail.Code, "id": fail.ID, "field": fail.Field})
}

// fillFromDocument gives each row what only the served document knows: the
// account's organization, which a new reading stores, and its configured
// refill date, which a reading is checked against when the row leaves the
// date to the config; and every credential, whose renewal dates are never
// evicted to make room.
func fillFromDocument(batch *overrides.Batch, doc aggregate.Document) {
	if doc.APICredits != nil {
		accounts := map[string]aggregate.APICreditAccount{}
		for _, account := range doc.APICredits.Accounts {
			accounts[account.ID] = account
		}
		for i, item := range batch.APICredits {
			if account, ok := accounts[item.ID]; ok {
				batch.APICredits[i].OrganizationID = account.OrganizationID
				batch.APICredits[i].ConfigRenews = account.Settings.ConfigRenews
			}
		}
	}
	batch.Credentials = map[string]bool{}
	for _, credential := range doc.Credentials {
		batch.Credentials[credential.ID] = true
	}
}

// notEditable is every row the served document does not offer for editing.
// Reading the document rather than the request is the point, as with a
// redeem: a caller cannot nominate an account the dashboard is not already
// offering. The one exception is an orphan, a row stored for an account no
// longer listed, which may be cleared and never set.
func notEditable(batch overrides.Batch, doc aggregate.Document) []string {
	refused := []string{}
	editable, orphans := map[string]bool{}, map[string]bool{}
	if doc.APICredits != nil {
		for _, account := range doc.APICredits.Accounts {
			editable[account.ID] = account.Settings.Editable
		}
		for _, orphan := range doc.APICredits.Orphans {
			orphans[orphan.ID] = true
		}
	}
	for _, item := range batch.APICredits {
		if !editable[item.ID] && !(orphans[item.ID] && item.Clears()) {
			refused = append(refused, item.ID)
		}
	}
	renewable, renewalOrphans := map[string]bool{}, map[string]bool{}
	for _, credential := range doc.Credentials {
		renewable[credential.ID] = credential.RenewalEditable
	}
	for _, orphan := range doc.RenewalOrphans {
		renewalOrphans[orphan.ID] = true
	}
	for _, item := range batch.Renewals {
		if !renewable[item.ID] && !(renewalOrphans[item.ID] && item.Clears()) {
			refused = append(refused, item.ID)
		}
	}
	return refused
}

// savedBlocks is each saved row as the rebuilt document now shows it: an
// account's settings block, or a credential's renewal setting; null for a row
// the document no longer lists, such as a cleared orphan.
func savedBlocks(batch overrides.Batch, doc aggregate.Document) map[string]any {
	blocks := map[string]any{}
	for _, item := range batch.APICredits {
		blocks[item.ID] = nil
		if account, ok := accountOf(doc, item.ID); ok {
			blocks[item.ID] = account.Settings
		}
	}
	for _, item := range batch.Renewals {
		blocks[item.ID] = nil
		if credential, ok := credentialOf(doc, item.ID); ok && credential.RenewalSetting != nil {
			blocks[item.ID] = credential.RenewalSetting
		}
	}
	return blocks
}

// currentBlocks is what each conflicting row holds now. The store answered
// with what it has stored, which the served document may not show yet when
// another save is still rebuilding, so the document's block is the frame and
// the stored values are laid over it.
func currentBlocks(conflict *overrides.ConflictError, doc aggregate.Document) map[string]any {
	blocks := map[string]any{}
	for _, id := range conflict.IDs {
		if conflict.Kind == overrides.KindRenewals {
			blocks[id] = nil
			if renewal, ok := conflict.Renewals[id]; ok {
				blocks[id] = &aggregate.RenewalSetting{
					Date: renewal.Date, Revision: overrides.RevisionText(renewal.Rev), UpdatedAtEpoch: renewal.UpdatedAt.Unix(),
				}
			}
			continue
		}
		block := aggregate.APICreditSettings{}
		if account, ok := accountOf(doc, id); ok {
			block = account.Settings
		}
		stored, has := conflict.APICredits[id]
		// The document's verdict on a reading only holds for the reading it
		// judged.
		if block.Revision != overrides.RevisionText(stored.Rev) {
			block.ReadingUnusedReason = ""
		}
		block.Revision = overrides.RevisionText(stored.Rev)
		block.MonthlyUSD, block.Renews, block.Reading, block.UpdatedAtEpoch = stored.MonthlyUSD, stored.Renews, nil, nil
		if r := stored.Reading; r != nil {
			block.Reading = &aggregate.StoredReading{RemainingUSD: r.RemainingUSD, AtEpoch: r.At.Unix(), EnteredAtEpoch: r.EnteredAt.Unix()}
		}
		if has {
			updated := stored.UpdatedAt.Unix()
			block.UpdatedAtEpoch = &updated
		}
		blocks[id] = block
	}
	return blocks
}

func accountOf(doc aggregate.Document, id string) (aggregate.APICreditAccount, bool) {
	if doc.APICredits == nil {
		return aggregate.APICreditAccount{}, false
	}
	for _, account := range doc.APICredits.Accounts {
		if account.ID == id {
			return account, true
		}
	}
	return aggregate.APICreditAccount{}, false
}

func credentialOf(doc aggregate.Document, id string) (aggregate.Credential, bool) {
	for _, credential := range doc.Credentials {
		if credential.ID == id {
			return credential, true
		}
	}
	return aggregate.Credential{}, false
}
