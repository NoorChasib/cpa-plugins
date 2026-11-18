package plugin

import (
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/credits"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/meter"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// laneSpec is one provider's API credit pipeline beside the snapshot: the
// organizations listed on the page, the meter that counts their traffic, the
// values saved for them, and the credit state worked out from all three.
// Claude's is the pipeline quota-cache has always run. OpenAI's runs beside it
// on files of its own with the same schemas, so no Claude file changes.
type laneSpec struct {
	meter    meter.Source
	settings *settings.Provider
	credits  *credits.Provider
	// meterPath and creditsPath are where the lane's meter file and credit
	// state sit beside the snapshot.
	meterPath, creditsPath func(snapshotPath string) string
	// watchesWithoutItems keeps the meter running while no organization is
	// listed. It then counts nothing and notes each organization it sees, so
	// the page can offer the first one to add: OpenAI's responses name an
	// organization in a form API Platform shows nowhere. Such a meter is never
	// stopped with no_items.
	watchesWithoutItems bool
	// optional publishes nothing while neither the lane's settings file nor
	// its organizations file exists, so an install that never lists an
	// organization of it gains no file beside the snapshot.
	optional bool
	// countsConfigItems publishes how many items claude-api-credits lists in
	// the configuration, which are ignored.
	countsConfigItems bool
	// renewals is whether the lane's settings hold the renewal dates of the
	// roster's Claude credentials, which the editor lists and the save route
	// checks against the roster.
	renewals bool
	// The lane's lines in CPA's log: a publish that failed, a settings file
	// that cannot be read, and an organizations save.
	publishWarning, settingsWarning, organizationsChanged string
}

// claudeSpec is the Claude API credit: Console organizations, Claude API-key
// traffic, and the files quota-cache 0.2.0 began keeping.
var claudeSpec = &laneSpec{
	meter: meter.Claude, settings: settings.Claude, credits: credits.Claude,
	meterPath: client.MeterPath, creditsPath: client.CreditsPath,
	countsConfigItems: true, renewals: true,
	publishWarning:       "quota-cache could not publish its credit state",
	settingsWarning:      "quota-cache cannot read its saved API credit settings; none apply until the file is moved aside",
	organizationsChanged: "quota-cache organizations changed",
}

// openAISpec is the OpenAI API credit: OpenAI API organizations as the
// openai-organization response header names them, openai-compatibility
// API-key traffic, and the snapshot.openai-*.json files.
var openAISpec = &laneSpec{
	meter: meter.OpenAI, settings: settings.OpenAI, credits: credits.OpenAI,
	meterPath: client.OpenAIMeterPath, creditsPath: client.OpenAICreditsPath,
	watchesWithoutItems: true, optional: true,
	publishWarning:       "quota-cache could not publish its OpenAI credit state",
	settingsWarning:      "quota-cache cannot read its saved OpenAI API credit settings; none apply until the file is moved aside",
	organizationsChanged: "quota-cache OpenAI organizations changed",
}

// lane is one provider's running parts.
type lane struct {
	spec *laneSpec
	// store, organizations and publisher run while the cache does, and are
	// nil while it is stopped: the values the operator saves on the page,
	// the organizations listed there, and the credit state worked out from
	// them. Each writes only while this plugin holds the cache's writer lock.
	// They are set and cleared under Plugin.mu.
	store         *settings.Store
	organizations *settings.OrganizationStore
	publisher     *publisher
	// meter counts the lane's API-key traffic per organization while it may
	// run (relinkLaneLocked); nil otherwise. usage.handle reads it without a
	// lock, since CPA delivers usage records on its own goroutine and must
	// never wait on a configure.
	meter atomic.Pointer[meter.Meter]
	// apiCredits is the item list in force, the organizations file's
	// (itemsOf), and itemsState is the organizations block published with
	// it. The fetcher reads Claude's on every scan, so a change on the page
	// needs no restart.
	apiCredits atomic.Pointer[[]creditItem]
	itemsState atomic.Pointer[client.CreditOrganizations]
	// linkOrganizations is the organizations store of the running cache, nil
	// while it is stopped. Guarded by Plugin.linkMu.
	linkOrganizations *settings.OrganizationStore
}

// laneParts are one lane's stores and publisher as the running cache held
// them when they were read under Plugin.mu, so a reader never meets a stop
// halfway through clearing them. All three are nil while the cache is
// stopped.
type laneParts struct {
	lane          *lane
	store         *settings.Store
	organizations *settings.OrganizationStore
	publisher     *publisher
}

// partsLocked is the lane's parts now. The caller holds Plugin.mu.
func (l *lane) partsLocked() laneParts {
	return laneParts{lane: l, store: l.store, organizations: l.organizations, publisher: l.publisher}
}

// lanes is every lane, Claude's first: each step of a configure or a stop acts
// on them in this order.
func (p *Plugin) lanes() [2]*lane { return [2]*lane{&p.claude, &p.openai} }

// itemsInForce is what the lane's publisher computes from: the item list and
// the organizations block published with it.
func (l *lane) itemsInForce() ([]creditItem, client.CreditOrganizations) {
	items, state := []creditItem{}, client.CreditOrganizations{State: client.OrganizationsStateMissing}
	if loaded := l.apiCredits.Load(); loaded != nil {
		items = *loaded
	}
	if loaded := l.itemsState.Load(); loaded != nil {
		state = *loaded
	}
	return items, state
}

// stopMeter stops the lane's running meter, if any, with reason, and forgets
// it. The caller holds Plugin.linkMu.
func (l *lane) stopMeter(reason string) {
	if m := l.meter.Swap(nil); m != nil {
		m.Stop(reason, time.Now().UTC())
	}
}
