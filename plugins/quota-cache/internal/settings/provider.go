package settings

import (
	"regexp"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Provider is one API credit's files and forms: where its settings and
// organizations files sit beside the snapshot, the account ids that key its
// settings, the form its organizations take, and what its meter may write in a
// reading's baseline. The two files have the same schemas for every provider;
// only these rules differ.
//
// Claude is the provider of every function without For in its name, so the
// Claude API credit's files load, save and compare exactly as they always
// have. OpenAI keeps the OpenAI API credit's files.
type Provider struct {
	settingsPath, organizationsPath func(snapshotPath string) string
	// accountKey is the form of an account id, which keys an API credit
	// entry. accountOf is the account id of an organization in the form
	// normalizeOrganization returns it.
	accountKey            *regexp.Regexp
	normalizeOrganization func(string) (string, bool)
	accountOf             func(string) string
	// prompts and tiers are the prompt classes and service tiers a baseline
	// entry may have. Claude's meter writes no tier, and Quota Glance 0.7 and
	// quota-cache 0.5.0 refused the key, so a Claude baseline entry with one
	// fails the file.
	prompts, tiers map[string]bool
	// renewals is whether the settings file may hold renewal dates. Only
	// Claude subscriptions have them; any in the OpenAI file fails it.
	renewals bool
	// organizationsKind is the kind of a save request for the organizations
	// card.
	organizationsKind string
}

// Claude is the Claude API credit: its Console organizations in
// snapshot.organizations.json and their values in snapshot.settings.json,
// byte-compatible with Quota Glance's settings.json.
var Claude = &Provider{
	settingsPath:          FilePath,
	organizationsPath:     client.OrganizationsPath,
	accountKey:            apiCreditKey,
	normalizeOrganization: client.NormalizeOrganizationID,
	accountOf:             client.APICreditOrgAccount,
	prompts:               map[string]bool{"": true, client.MeterPromptOver100K: true},
	tiers:                 map[string]bool{"": true},
	renewals:              true,
	organizationsKind:     KindOrganizations,
}

// OpenAI is the OpenAI API credit: its organizations, each named as the
// openai-organization response header names it, in
// snapshot.openai-organizations.json, and their values in
// snapshot.openai-settings.json, which has the settings file's schema and
// never a renewal date.
var OpenAI = &Provider{
	settingsPath:          client.OpenAISettingsPath,
	organizationsPath:     client.OpenAIOrganizationsPath,
	accountKey:            openAICreditKey,
	normalizeOrganization: client.NormalizeOpenAIOrganization,
	accountOf:             client.OpenAICreditOrgAccount,
	prompts:               map[string]bool{"": true, client.MeterPromptOver272K: true},
	tiers: map[string]bool{"": true, client.MeterTierFlex: true, client.MeterTierFast: true,
		client.MeterTierUltrafast: true, client.MeterTierUnknown: true},
	renewals:          false,
	organizationsKind: KindOpenAIOrganizations,
}

// openAICreditKey is an OpenAI account id, client.OpenAICreditOrgAccount's
// form.
var openAICreditKey = regexp.MustCompile(`^oai-[0-9a-f]{12}$`)

// AccountOf is the organization's account id: client.APICreditOrgAccount for
// Claude, client.OpenAICreditOrgAccount for OpenAI.
func (p *Provider) AccountOf(o Organization) string { return p.accountOf(o.OrganizationID) }

// ValidAPICreditID reports whether id can key one of p's API credit entries.
func (p *Provider) ValidAPICreditID(id string) bool { return p.accountKey.MatchString(id) }

// OrganizationsPath is where p's organizations file sits beside the snapshot
// at snapshotPath.
func (p *Provider) OrganizationsPath(snapshotPath string) string {
	return p.organizationsPath(snapshotPath)
}
