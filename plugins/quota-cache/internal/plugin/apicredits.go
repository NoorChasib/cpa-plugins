package plugin

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"gopkg.in/yaml.v3"
)

// creditItem is one entry of claude-api-credits: a Claude Console
// organization whose monthly API credit is tracked. It holds nothing but what
// reaches the snapshot; the organization id links it to the meter, and no key
// is read from it.
type creditItem struct {
	id     string
	credit client.APICredit
}

const maxCreditLabelRunes = 64

// creditFields are the keys an item may have. admin-key is accepted so a
// 0.1.13 configuration still loads; its value is dropped unread.
var creditFields = []string{"label", "organization-id", "monthly-usd", "renews", "admin-key"}

// parseAPICredits judges each configured item on its own, so one bad item
// never stops the others: every item becomes an account, and an item with a
// problem is listed with the problem and never counted. Only the first
// problem found is kept, in the order of the CreditProblem values, and every
// field that is valid is still filled in. monthly-usd and renews are
// optional: an invalid value is ignored and flagged, never a problem.
//
// Values are read as the scalar's text, never as a typed YAML value, so an
// unquoted date or amount arrives as written. A value YAML resolves to null
// (empty, ~, null, or !!null) counts as missing. That includes the !!null
// scalar whose text is "null" that CPA's plugin panel writes for a JSON null,
// which would otherwise make "null" a label. A quoted "null" is a string.
func parseAPICredits(nodes []yaml.Node) []creditItem {
	items := make([]creditItem, 0, len(nodes))
	labels, organizations := map[string]bool{}, map[string]bool{}
	for index := range nodes {
		item := creditItem{credit: client.APICredit{Position: index}}
		problem := ""
		note := func(code string) {
			if problem == "" {
				problem = code
			}
		}
		values := map[string]string{}
		node := resolveAlias(&nodes[index])
		if node == nil || node.Kind != yaml.MappingNode {
			note(client.CreditProblemItemInvalid)
		} else {
			for i := 0; i+1 < len(node.Content); i += 2 {
				name, value := resolveAlias(node.Content[i]), resolveAlias(node.Content[i+1])
				if name == nil || name.Kind != yaml.ScalarNode || value == nil || value.Kind != yaml.ScalarNode {
					note(client.CreditProblemItemInvalid)
					continue
				}
				if _, repeated := values[name.Value]; repeated {
					// Which of two spellings counts is not ours to guess.
					note(client.CreditProblemItemInvalid)
					continue
				}
				if name.Value == "admin-key" {
					// Present is all that is read of it: the value is never
					// validated, fingerprinted, stored, logged or formatted.
					values[name.Value] = ""
					item.credit.AdminKeyIgnored = true
					continue
				}
				text := strings.TrimSpace(value.Value)
				if value.ShortTag() == "!!null" {
					text = ""
				}
				values[name.Value] = text
			}
			for name := range values {
				if !known(name) {
					note(client.CreditProblemUnknownField)
				}
			}
		}
		if index >= client.MaxAPICreditItems {
			note(client.CreditProblemTooManyItems)
		}

		switch label := values["label"]; {
		case label == "":
			note(client.CreditProblemLabelMissing)
		case !utf8.ValidString(label) || utf8.RuneCountInString(label) > maxCreditLabelRunes || strings.IndexFunc(label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
			note(client.CreditProblemLabelInvalid)
		case labels[strings.ToLower(label)]:
			note(client.CreditProblemLabelDuplicate)
		default:
			labels[strings.ToLower(label)] = true
			item.credit.Label = label
		}

		// The id is set whenever its shape is valid, whatever else is wrong,
		// so a consumer can name a duplicate or an over-limit organization.
		linked := false
		switch organization, valid := client.NormalizeOrganizationID(values["organization-id"]); {
		case values["organization-id"] == "":
			note(client.CreditProblemOrganizationIDMissing)
		case !valid:
			note(client.CreditProblemOrganizationIDInvalid)
		default:
			item.credit.OrganizationID = organization
			if organizations[organization] {
				note(client.CreditProblemOrganizationIDDuplicate)
			} else {
				organizations[organization] = true
				linked = index < client.MaxAPICreditItems
			}
		}

		if monthly := values["monthly-usd"]; monthly != "" {
			if client.ValidMonthlyUSD(monthly) {
				item.credit.MonthlyUSD = monthly
			} else {
				item.credit.MonthlyUSDInvalid = true
			}
		}
		if renews := values["renews"]; renews != "" {
			if _, err := client.ParseRenewal(renews); err == nil {
				item.credit.Renews = renews
			} else {
				item.credit.RenewsInvalid = true
			}
		}

		item.credit.Problem = problem
		item.id = "item-" + strconv.Itoa(index+1)
		if linked {
			item.id = client.APICreditOrgAccount(item.credit.OrganizationID)
		}
		items = append(items, item)
	}
	return items
}

func known(name string) bool {
	for _, field := range creditFields {
		if name == field {
			return true
		}
	}
	return false
}

// linkedOrganizations is what the meter counts: the organization of every
// item keyed by it, which is the first valid occurrence of each id among the
// first MaxAPICreditItems items, whatever else is wrong with the item. Fixing
// a label therefore loses no history.
func linkedOrganizations(items []creditItem) []string {
	linked := []string{}
	for _, item := range items {
		if strings.HasPrefix(item.id, "org-") {
			linked = append(linked, item.credit.OrganizationID)
		}
	}
	return linked
}

// adminKeysIgnored counts the items that still carry admin-key.
func adminKeysIgnored(items []creditItem) int {
	n := 0
	for _, item := range items {
		if item.credit.AdminKeyIgnored {
			n++
		}
	}
	return n
}

// resolveAlias follows a YAML alias to the node it names. Anchors cannot form
// a cycle in a parsed document, but the walk is bounded anyway.
func resolveAlias(node *yaml.Node) *yaml.Node {
	for i := 0; node != nil && node.Kind == yaml.AliasNode && i < 8; i++ {
		node = node.Alias
	}
	if node != nil && node.Kind == yaml.AliasNode {
		return nil
	}
	return node
}
