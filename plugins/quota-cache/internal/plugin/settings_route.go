package plugin

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/credits"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// The page's editor: what the status route tells it, and the save route it
// sends to.
//
// The save route is a POST under CPA's management tree, so CPA has checked the
// management key, which it takes from a header only, before the plugin sees
// the request. There is no door on the resource tree: the page always holds
// the console's key. The page sends a save only from a button press, and
// never again after a 401 or 403.
const (
	settingsPath = "/v0/management/plugins/" + ID + "/settings"
	// maxSettingsBytes bounds a request's body. Sixteen API credit rows are
	// about 190 bytes each, and sixteen organization operations fit as well.
	maxSettingsBytes = 4096
	// organizationsUnreadable is the organizations editor's reason while its
	// file cannot be read.
	organizationsUnreadable = "organizationsUnreadable"
)

// claudeCredential is one Claude credential on CPA's roster, as the page's
// renewal dates need it. Its name is held in memory only: it is never written
// to a file or a log, and leaves the plugin only through the authenticated
// status route.
type claudeCredential struct {
	AuthIndex string
	Name      string
	Disabled  bool
}

// credentialName is the address a credential shows as: the one its auth
// index spells, as for claude-<address>.json, else the address CPA reports,
// else its label, else the auth index itself.
func credentialName(entry protocol.HostAuthFileEntry, provider string) string {
	derived := strings.TrimPrefix(strings.TrimSuffix(entry.AuthIndex, ".json"), provider+"-")
	switch {
	case strings.Contains(derived, "@"):
		return derived
	case entry.Email != "":
		return entry.Email
	case strings.TrimSpace(entry.Label) != "":
		return strings.TrimSpace(entry.Label)
	}
	return derived
}

// editorStatus is the status route's editor key: whether and what the page
// may edit, and what only the plugin knows about the rows. The API credit rows
// themselves are credit_state's accounts.
type editorStatus struct {
	// Available is whether the saved settings can be edited, and Reason why
	// not: client.CreditEditingSettingsUnreadable.
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	// Revision is the settings file's, "" when there is none.
	Revision          string             `json:"revision"`
	MaxAPICreditItems int                `json:"max_api_credit_items"`
	MaxRenewalItems   int                `json:"max_renewal_items"`
	MaxBatchBytes     int                `json:"max_batch_bytes"`
	Credentials       []editorCredential `json:"credentials"`
	RenewalOrphans    []renewalOrphan    `json:"renewal_orphans"`
	// APICreditOrphans are the saved API credit values while the published
	// state lists no organization. api_credits is null then and carries no
	// orphans, yet the values of a removed last organization must still be
	// shown and cleared. Empty while api_credits is published: its own
	// orphans are the list.
	APICreditOrphans []client.CreditOrphan `json:"api_credit_orphans"`
	// HeldAPICredits are the saved API credit values while the organizations
	// file cannot be read. No organization is listed then, and any value may
	// belong to one in the file, so none is an orphan: the page shows them
	// without offering to clear them, and the save route refuses to touch
	// them until the file can be read. Empty otherwise.
	HeldAPICredits []client.CreditOrphan `json:"held_api_credits"`
	Organizations  editorOrganizations   `json:"organizations"`
}

// editorCredential is one Claude credential on the roster, disabled ones
// included, with its renewal as the shared rule gives it now.
type editorCredential struct {
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Disabled  bool   `json:"disabled"`
	// BillingPeriod is the snapshot's: monthly, annual, or "" when unknown.
	BillingPeriod string `json:"billing_period"`
	// RenewalEditable is whether a renewal date may be saved for it.
	RenewalEditable bool `json:"renewal_editable"`
	// Renewal is the date saved for it, null when there is none.
	Renewal *client.RenewalSetting `json:"renewal"`
	// RenewalAtEpoch and RenewalSource are client.SubscriptionRenewal's: null
	// and "" when nothing gives a renewal.
	RenewalAtEpoch *int64 `json:"renewal_at_epoch"`
	RenewalSource  string `json:"renewal_source"`
	// RenewalEstimateAtEpoch is the estimate a saved date replaces.
	RenewalEstimateAtEpoch *int64 `json:"renewal_estimate_at_epoch"`
}

// renewalOrphan is a saved renewal date whose credential the roster no
// longer lists. It carries no name, so the page shows a shortened auth index.
type renewalOrphan struct {
	ID             string `json:"id"`
	Date           string `json:"date"`
	Revision       string `json:"revision"`
	UpdatedAtEpoch int64  `json:"updatedAtEpoch"`
}

// editorOrganizations is the organizations card's: the list in the
// organizations file, and whether it can be edited.
type editorOrganizations struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	// Revision is the file's, "" when there is none: a batch's baseRevision.
	Revision string             `json:"revision"`
	MaxItems int                `json:"max_items"`
	MaxOps   int                `json:"max_ops"`
	Items    []organizationItem `json:"items"`
}

// organizationItem is one organization in the file.
type organizationItem struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	OrganizationID string `json:"organization_id"`
}

// editorOf is the status route's editor key, from the stores as they are now
// and the state last published, which is what the save route judges a request
// against.
func (p *Plugin) editorOf(store *settings.Store, organizations *settings.OrganizationStore, state *client.CreditState, snapshot client.Snapshot, now time.Time) editorStatus {
	values, status := store.Snapshot()
	editor := editorStatus{
		Available: status.Editable(), Reason: editingReasonOf(status),
		Revision: settings.RevisionText(values.Revision), MaxAPICreditItems: settings.MaxAPICreditItems,
		MaxRenewalItems: settings.MaxRenewalItems, MaxBatchBytes: maxSettingsBytes,
		Credentials: []editorCredential{}, RenewalOrphans: []renewalOrphan{}, APICreditOrphans: []client.CreditOrphan{},
		HeldAPICredits: []client.CreditOrphan{}, Organizations: organizationsEditorOf(organizations.Current()),
	}
	if status.State == client.CreditSettingsOK {
		editor.APICreditOrphans = apiCreditOrphansOf(state, values)
		editor.HeldAPICredits = heldAPICreditsOf(state, values)
	}
	roster := p.roster.Load()
	if roster == nil {
		return editor
	}
	for _, c := range *roster {
		entry, has := snapshot.Entries[client.Key("claude", c.AuthIndex)]
		var observed *client.Entry
		if has {
			observed = &entry
		}
		credential := editorCredential{
			AuthIndex: c.AuthIndex, Name: c.Name, Disabled: c.Disabled,
			RenewalEditable:        editor.Available && client.ValidRenewalID(c.AuthIndex),
			RenewalEstimateAtEpoch: epochOf(client.SubscriptionRenewalEstimate("claude", observed, now)),
		}
		if observed != nil && observed.Quota != nil {
			credential.BillingPeriod = observed.Quota.BillingPeriod
		}
		date := ""
		if stored, ok := values.Renewals[c.AuthIndex]; ok {
			credential.Renewal, date = renewalSettingOf(stored), stored.Date
		}
		at, source := client.SubscriptionRenewal("claude", observed, date, now)
		credential.RenewalAtEpoch, credential.RenewalSource = epochOf(at), source
		editor.Credentials = append(editor.Credentials, credential)
	}
	editor.RenewalOrphans = renewalOrphansOf(values, *roster)
	return editor
}

// editingReasonOf is the editor's reason while status is not editable.
func editingReasonOf(status settings.Status) string {
	if status.State == client.CreditSettingsUnreadable {
		return client.CreditEditingSettingsUnreadable
	}
	return ""
}

func organizationsEditorOf(o settings.Organizations) editorOrganizations {
	out := editorOrganizations{
		Available: o.State != client.OrganizationsStateUnreadable, Revision: settings.RevisionText(o.Revision),
		MaxItems: settings.MaxOrganizations, MaxOps: settings.MaxOrganizationOps, Items: organizationItemsOf(o),
	}
	if !out.Available {
		out.Reason = organizationsUnreadable
	}
	return out
}

func organizationItemsOf(o settings.Organizations) []organizationItem {
	items := make([]organizationItem, 0, len(o.Items))
	for _, item := range o.Items {
		items = append(items, organizationItem{ID: item.ID(), Label: item.Label, OrganizationID: item.OrganizationID})
	}
	return items
}

// renewalOrphansOf is every saved renewal date whose credential the roster
// does not list, oldest first.
func renewalOrphansOf(values settings.Values, roster []claudeCredential) []renewalOrphan {
	listed := map[string]bool{}
	for _, c := range roster {
		listed[c.AuthIndex] = true
	}
	out := []renewalOrphan{}
	for id, renewal := range values.Renewals {
		if !listed[id] {
			out = append(out, renewalOrphan{ID: id, Date: renewal.Date, Revision: settings.RevisionText(renewal.Rev), UpdatedAtEpoch: renewal.UpdatedAt.Unix()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAtEpoch != out[j].UpdatedAtEpoch {
			return out[i].UpdatedAtEpoch < out[j].UpdatedAtEpoch
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// apiCreditOrphansOf is every saved API credit row while state lists no
// organization, when each of them is an orphan that api_credits, being null,
// cannot carry. Empty while api_credits is published, before the first
// state, and while the organizations file cannot be read, when the rows are
// held instead (heldAPICreditsOf).
func apiCreditOrphansOf(state *client.CreditState, values settings.Values) []client.CreditOrphan {
	if state == nil || state.APICredits != nil || values.Unreadable || organizationsUnread(state) {
		return []client.CreditOrphan{}
	}
	return credits.Orphans(values)
}

// heldAPICreditsOf is every saved API credit row while the organizations file
// cannot be read, oldest first: what would otherwise be orphans, though they
// may belong to organizations in the file. No organization is listed then,
// so that is every saved row.
func heldAPICreditsOf(state *client.CreditState, values settings.Values) []client.CreditOrphan {
	if !organizationsUnread(state) || values.Unreadable {
		return []client.CreditOrphan{}
	}
	return credits.Orphans(values)
}

// organizationsUnread reports whether state was published while the
// organizations file could not be read.
func organizationsUnread(state *client.CreditState) bool {
	return state != nil && state.Organizations.State == client.OrganizationsStateUnreadable
}

func renewalSettingOf(r settings.Renewal) *client.RenewalSetting {
	return &client.RenewalSetting{Date: r.Date, Revision: settings.RevisionText(r.Rev), UpdatedAtEpoch: r.UpdatedAt.Unix()}
}

func epochOf(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	epoch := t.Unix()
	return &epoch
}

// saveSettings is the save route. Its checks run in a fixed order, and every
// answer is JSON with Cache-Control: no-store:
//
//  1. the cache is stopped: 503 cache_stopped;
//  2. Sec-Fetch-Site is sent and is not same-origin: 403 cross_site;
//  3. the body is not application/json: 415 unsupported_media_type;
//  4. the body is over maxSettingsBytes: 400 invalid_request;
//  5. what the request's kind needs cannot be read: 503
//     organizations_unavailable for the organizations card, else 503
//     settings_unavailable;
//  6. then the kind's own rules.
//
// Each request kind locks one store at a time: the store's own lock inside its
// Apply, then linkMu for an organizations change, then the publisher's lock
// to publish, each released before the next is taken.
func (p *Plugin) saveSettings(req protocol.ManagementRequest) protocol.ManagementResponse {
	p.mu.Lock()
	running, store, organizations, publisher := p.cache != nil, p.store, p.organizations, p.publisher
	p.mu.Unlock()
	switch {
	case !running:
		return response(http.StatusServiceUnavailable, map[string]string{"error": "cache_stopped"})
	case crossSite(req.Headers):
		// Defence in depth: CPA's key is in a header, which a cross-site page
		// cannot send without asking.
		return response(http.StatusForbidden, map[string]string{"error": "cross_site"})
	case !jsonMedia(req.Headers):
		return response(http.StatusUnsupportedMediaType, map[string]string{"error": "unsupported_media_type"})
	case len(req.Body) > maxSettingsBytes:
		return response(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	now := time.Now().UTC()
	kind := settings.RequestKind(req.Body)
	if kind == settings.KindOrganizations {
		return p.saveOrganizations(organizations, publisher, req.Body, now)
	}
	if !store.Status().Editable() {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "settings_unavailable"})
	}
	return p.saveBatch(store, publisher, req.Body, now)
}

// crossSite reports a browser saying the request does not come from a page on
// this origin. A request with no Sec-Fetch-Site, from curl or a script, is not
// refused for it.
func crossSite(headers http.Header) bool {
	for _, value := range headers.Values("Sec-Fetch-Site") {
		if !strings.EqualFold(strings.TrimSpace(value), "same-origin") {
			return true
		}
	}
	return false
}

// jsonMedia reports whether the body is declared JSON. A form-encoded body is
// the one a cross-site form can send without asking, so nothing else is read.
func jsonMedia(headers http.Header) bool {
	media, _, _ := strings.Cut(headers.Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(media), "application/json")
}

// saveBatch is one API credit or renewal card's Save: its shape, its values,
// whether a row waits on the organizations file, whether the published state
// offers every row it names, and then the store, which takes it all or
// nothing, then a publish before the answer.
func (p *Plugin) saveBatch(store *settings.Store, publisher *publisher, raw []byte, now time.Time) protocol.ManagementResponse {
	batch, err := settings.ParseBatch(raw)
	if err != nil {
		return response(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	state := publisher.LastState()
	if state == nil {
		publisher.PublishNow()
		state = publisher.LastState()
	}
	roster := []claudeCredential{}
	rosterRead := p.roster.Load()
	if rosterRead != nil {
		roster = *rosterRead
	}
	fillFromState(&batch, state, roster)
	stored := store.Current()
	if fail := batch.Check(stored, now); fail != nil {
		return fieldRefusal(fail)
	}
	if heldByOrganizations(batch, state) {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "organizations_unavailable"})
	}
	if refused := notEditable(batch, state, stored, rosterRead); len(refused) > 0 {
		return notEditableRefusal(refused)
	}
	result, err := store.Apply(batch, publisher.LastMeter(), now)
	var conflict *settings.ConflictError
	var field *settings.FieldError
	var refused *settings.NotEditableError
	switch {
	case errors.As(err, &conflict):
		return response(http.StatusConflict, map[string]any{
			"error": "conflict", "revision": conflict.Revision, "conflicts": conflict.IDs, "current": currentBlocks(conflict, publisher.LastState()),
		})
	case errors.As(err, &field):
		return fieldRefusal(field)
	case errors.As(err, &refused):
		return notEditableRefusal(refused.IDs)
	case err != nil:
		return storeRefusal(err)
	}
	revision := settings.RevisionText(result.Revision)
	if result.Unchanged {
		return response(http.StatusOK, map[string]any{"ok": true, "unchanged": true, "revision": revision})
	}
	// Published before the answer, so the page's next read shows the save and
	// its next save starts from these revisions.
	publisher.PublishNow()
	p.host.Log(context.Background(), "info", "quota-cache settings changed", map[string]any{"kind": batch.Kind, "ids": result.IDs, "fields": result.Fields})
	return response(http.StatusOK, map[string]any{
		"ok": true, "unchanged": false, "revision": revision, "settings": savedBlocks(batch, publisher.LastState()),
	})
}

// saveOrganizations is the organizations card's Save. The file is committed
// under its store's lock alone; then, under linkMu, the item list and the
// meter follow it; then the state is published, before the answer. They
// follow the file whatever the batch comes to when Apply read it again.
func (p *Plugin) saveOrganizations(organizations *settings.OrganizationStore, publisher *publisher, raw []byte, now time.Time) protocol.ManagementResponse {
	// The file as it is now, as the next tick would see it: one moved aside
	// or deleted is gone, so a batch sent in the seconds before that tick
	// conflicts with the empty list instead of writing over a list the page
	// no longer shows, and one that could not be opened is read again. What
	// that changed is published here, whatever the batch comes to: that tick
	// has nothing left to follow, and a file that opened again once its owner
	// was fixed changed none of the stamps the publisher compares.
	if p.followOrganizations(organizations, now) {
		publisher.PublishNow()
	}
	if organizations.Current().State == client.OrganizationsStateUnreadable {
		return response(http.StatusServiceUnavailable, map[string]string{"error": "organizations_unavailable"})
	}
	batch, err := settings.ParseOrganizationsBatch(raw)
	if err != nil {
		return response(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	p.step("apply organizations")
	result, err := organizations.Apply(batch, now)
	saved := err == nil && !result.Unchanged
	// Apply reads the file again first when it changed since the
	// followOrganizations above, and the store keeps what it read, whatever
	// the batch comes to. The item list and the meter follow it here, as the
	// next tick would.
	followed := p.followOrganizations(organizations, now)
	if saved && !followed {
		p.linkMu.Lock()
		// A stop that began meanwhile has let go of this run's stores, and
		// the next start lists the items from the file.
		if p.linkOrganizations == organizations {
			p.relinkLocked(now, false)
		}
		p.linkMu.Unlock()
	}
	if saved || followed {
		publisher.PublishNow()
	}
	var conflict *settings.OrganizationsConflictError
	var op *settings.OpError
	switch {
	case errors.As(err, &conflict):
		return response(http.StatusConflict, map[string]any{
			"error": "conflict", "revision": conflict.Revision, "organizations": organizationItemsOf(conflict.Organizations),
		})
	case errors.As(err, &op):
		return response(http.StatusBadRequest, map[string]any{"error": op.Code, "op": op.Op, "field": op.Field})
	case errors.Is(err, settings.ErrOrganizationsUnavailable):
		return response(http.StatusServiceUnavailable, map[string]string{"error": "organizations_unavailable"})
	case err != nil:
		return storeRefusal(err)
	}
	if saved {
		p.host.Log(context.Background(), "info", "quota-cache organizations changed", map[string]any{"added": result.Added, "renamed": result.Renamed, "removed": result.Removed})
	}
	return response(http.StatusOK, map[string]any{
		"ok": true, "unchanged": result.Unchanged, "revision": settings.RevisionText(result.Revision),
		"organizations": organizationItemsOf(organizations.Current()),
	})
}

// storeRefusal answers a store's own failure.
func storeRefusal(err error) protocol.ManagementResponse {
	switch {
	case errors.Is(err, settings.ErrClosed):
		return response(http.StatusServiceUnavailable, map[string]string{"error": "cache_stopped"})
	case errors.Is(err, settings.ErrUnavailable):
		return response(http.StatusServiceUnavailable, map[string]string{"error": "settings_unavailable"})
	case errors.Is(err, settings.ErrTooManyWrites):
		res := response(http.StatusTooManyRequests, map[string]string{"error": "too_many_writes"})
		res.Headers.Set("Retry-After", "60")
		return res
	case errors.Is(err, settings.ErrFull):
		return response(http.StatusConflict, map[string]string{"error": "settings_full"})
	}
	return response(http.StatusServiceUnavailable, map[string]string{"error": "settings_unwritable"})
}

func fieldRefusal(fail *settings.FieldError) protocol.ManagementResponse {
	return response(http.StatusBadRequest, map[string]string{"error": fail.Code, "id": fail.ID, "field": fail.Field})
}

// notEditableRefusal names the refused rows: id is the first, and ids all of
// them, in request order.
func notEditableRefusal(ids []string) protocol.ManagementResponse {
	return response(http.StatusConflict, map[string]any{"error": "not_editable", "id": ids[0], "ids": ids})
}

// fillFromState gives each row what only the published state knows: the
// account's organization, which a new reading stores; and every Claude
// credential on the roster, whose renewal dates are never evicted to make
// room. Nothing of it comes from the request.
func fillFromState(batch *settings.Batch, state *client.CreditState, roster []claudeCredential) {
	if state != nil && state.APICredits != nil {
		accounts := map[string]client.CreditAccount{}
		for _, account := range state.APICredits.Accounts {
			accounts[account.ID] = account
		}
		for i, item := range batch.APICredits {
			if account, ok := accounts[item.ID]; ok {
				batch.APICredits[i].OrganizationID = account.OrganizationID
			}
		}
	}
	batch.Credentials = map[string]bool{}
	for _, c := range roster {
		batch.Credentials[c.AuthIndex] = true
	}
}

// heldByOrganizations reports whether batch names any API credit row while
// the organizations file cannot be read. No organization is listed then, so
// every row is held: it may belong to an organization in the file, so it is
// neither set nor cleared until the file can be read. A clear would delete
// values that are still in use, and nothing could bring back a reading older
// than 48 hours with its time.
func heldByOrganizations(batch settings.Batch, state *client.CreditState) bool {
	return organizationsUnread(state) && len(batch.APICredits) > 0
}

// notEditable is every row the published state does not offer for editing.
// Reading the state rather than the request is the point: a caller cannot
// nominate an account the page is not already offering. The one exception is
// an orphan, a row stored for an account or credential no longer listed,
// which may be cleared and never set. While the state lists no organization,
// every stored API credit row is one, unless the organizations file cannot be
// read, when heldByOrganizations has refused the batch first. roster is nil
// before the first roster read, when no stored date is known to be an orphan.
func notEditable(batch settings.Batch, state *client.CreditState, stored settings.Values, roster *[]claudeCredential) []string {
	refused := []string{}
	editable, orphans := map[string]bool{}, map[string]bool{}
	if state != nil && state.APICredits != nil {
		for _, account := range state.APICredits.Accounts {
			editable[account.ID] = account.Settings.Editable
		}
		for _, orphan := range state.APICredits.Orphans {
			orphans[orphan.ID] = true
		}
	}
	for _, orphan := range apiCreditOrphansOf(state, stored) {
		orphans[orphan.ID] = true
	}
	for _, item := range batch.APICredits {
		if !editable[item.ID] && !(orphans[item.ID] && item.Clears()) {
			refused = append(refused, item.ID)
		}
	}
	renewable, renewalOrphans := map[string]bool{}, map[string]bool{}
	if roster != nil {
		for _, c := range *roster {
			renewable[c.AuthIndex] = client.ValidRenewalID(c.AuthIndex)
		}
		for _, orphan := range renewalOrphansOf(stored, *roster) {
			renewalOrphans[orphan.ID] = true
		}
	}
	for _, item := range batch.Renewals {
		if !renewable[item.ID] && !(renewalOrphans[item.ID] && item.Clears()) {
			refused = append(refused, item.ID)
		}
	}
	return refused
}

// savedBlocks is each saved row as the newly published state shows it: an
// account's settings block, or a credential's renewal date; null for a row the
// state no longer lists, such as a cleared orphan or a cleared date.
func savedBlocks(batch settings.Batch, state *client.CreditState) map[string]any {
	blocks := map[string]any{}
	for _, item := range batch.APICredits {
		blocks[item.ID] = nil
		if account, ok := accountOf(state, item.ID); ok {
			blocks[item.ID] = account.Settings
		}
	}
	for _, item := range batch.Renewals {
		blocks[item.ID] = nil
		if state != nil {
			if renewal, ok := state.Renewals[item.ID]; ok {
				blocks[item.ID] = renewal
			}
		}
	}
	return blocks
}

// currentBlocks is what each conflicting row holds now. The store answered
// with what it has stored, which the published state may not show yet when
// another save is still publishing, so the state's block is the frame and the
// stored values are laid over it.
func currentBlocks(conflict *settings.ConflictError, state *client.CreditState) map[string]any {
	blocks := map[string]any{}
	for _, id := range conflict.IDs {
		if conflict.Kind == settings.KindRenewals {
			blocks[id] = nil
			if renewal, ok := conflict.Renewals[id]; ok {
				blocks[id] = renewalSettingOf(renewal)
			}
			continue
		}
		block := client.CreditSettings{}
		if account, ok := accountOf(state, id); ok {
			block = account.Settings
		}
		stored, has := conflict.APICredits[id]
		// The state's verdict on a reading only holds for the reading it
		// judged.
		if block.Revision != settings.RevisionText(stored.Rev) {
			block.ReadingUnusedReason = ""
		}
		block.Revision = settings.RevisionText(stored.Rev)
		block.MonthlyUSD, block.Renews, block.Reading, block.UpdatedAtEpoch = stored.MonthlyUSD, stored.Renews, nil, nil
		if r := stored.Reading; r != nil {
			block.Reading = &client.StoredReading{RemainingUSD: r.RemainingUSD, AtEpoch: r.At.Unix(), EnteredAtEpoch: r.EnteredAt.Unix()}
		}
		if has {
			updated := stored.UpdatedAt.Unix()
			block.UpdatedAtEpoch = &updated
		}
		blocks[id] = block
	}
	return blocks
}

func accountOf(state *client.CreditState, id string) (client.CreditAccount, bool) {
	if state == nil || state.APICredits == nil {
		return client.CreditAccount{}, false
	}
	for _, account := range state.APICredits.Accounts {
		if account.ID == id {
			return account, true
		}
	}
	return client.CreditAccount{}, false
}
