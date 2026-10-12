package plugin

import (
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// creditItem is one Claude Console organization on quota-cache's page whose
// monthly API credit is tracked. It holds nothing but what reaches the
// snapshot; the organization id links it to the meter, and no key is read
// from it.
type creditItem struct {
	id     string
	credit client.APICredit
}

// linkedOrganizations is what the meter counts: the organization of every
// item.
func linkedOrganizations(items []creditItem) []string {
	linked := make([]string, 0, len(items))
	for _, item := range items {
		linked = append(linked, item.credit.OrganizationID)
	}
	return linked
}

// itemsOf is the item list quota-cache counts and lists, with the credit
// state's organizations block: the organizations file's, and nothing else.
// With no file there is no organization yet; with one that cannot be read
// there is none either, and a running meter stops as when the last one is
// removed.
// configItems is how many items claude-api-credits lists in the
// configuration, which are ignored.
func itemsOf(orgs settings.Organizations, configItems int) ([]creditItem, client.CreditOrganizations) {
	block := client.CreditOrganizations{State: orgs.State, Revision: settings.RevisionText(orgs.Revision), ConfigItemsIgnored: configItems}
	if block.State == "" {
		block.State = client.OrganizationsStateMissing
	}
	items := make([]creditItem, 0, len(orgs.Items))
	for _, credit := range orgs.APICredits() {
		items = append(items, creditItem{id: client.APICreditOrgAccount(credit.OrganizationID), credit: credit})
	}
	return items, block
}
