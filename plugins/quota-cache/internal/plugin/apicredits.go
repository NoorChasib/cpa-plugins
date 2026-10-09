package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/cache"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/quota"
	"gopkg.in/yaml.v3"
)

// creditItem is one entry of claude-api-credits: a Claude Console
// organization whose monthly API credit is tracked. key is its Admin API key,
// which is only ever put in a request header; credit, the part that reaches
// the snapshot, carries a fingerprint of it instead.
type creditItem struct {
	id     string
	credit client.APICredit
	key    string
}

// Format prints an item with its key replaced by the key's fingerprint, for
// every verb (%v, %+v, %#v, %s, %d, ...), so formatting an item, a slice of
// them or a pointer to one in a log, an error, a panic or a test failure can
// never print the key. fmt does not call methods on unexported fields, so
// this has to be on the item rather than on a type for key.
func (i creditItem) Format(state fmt.State, _ rune) {
	key := "none"
	if i.key != "" {
		key = creditKeyFingerprint(i.key)
	}
	fmt.Fprintf(state, "{id:%s credit:%+v key:%s}", i.id, i.credit, key)
}

// An Anthropic key's shape: the sk-ant- prefix, the characters Anthropic keys
// use, and a length that keeps the header small. Whether the key may read the
// cost report is Anthropic's to say, with 401 or 403; this accepts Admin API
// keys (sk-ant-admin01-...) and the personal and service-account keys
// (sk-ant-api03-...) the Admin API also takes.
var anthropicKeyShape = regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{10,250}$`)

const maxCreditLabelRunes = 64

// creditFields are the keys an item may have, in the order they are judged.
var creditFields = []string{"label", "admin-key", "monthly-usd", "renews"}

// parseAPICredits judges each configured item on its own, so one bad item
// never stops the others: every item becomes an account, and an item with a
// problem is listed with the problem and never polled. Only the first problem
// found is kept, in the order of the CreditProblem values, and every field
// that is valid is still filled in.
//
// Values are read as the scalar's text, never as a typed YAML value, so an
// unquoted date or amount arrives as written. A value YAML resolves to null
// (empty, ~, null, or !!null) counts as missing. That includes the !!null
// scalar whose text is "null" that CPA's plugin panel writes for a JSON null,
// which would otherwise make "null" a label. A quoted "null" is a string.
func parseAPICredits(nodes []yaml.Node) []creditItem {
	items := make([]creditItem, 0, len(nodes))
	labels, keys := map[string]bool{}, map[string]bool{}
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

		switch key := values["admin-key"]; {
		case key == "":
			note(client.CreditProblemAdminKeyMissing)
		case !anthropicKeyShape.MatchString(key):
			note(client.CreditProblemAdminKeyInvalid)
		default:
			if keys[key] {
				note(client.CreditProblemAdminKeyRepeated)
			}
			keys[key] = true
			item.key = key
			item.credit.KeyFingerprint = creditKeyFingerprint(key)
		}

		switch monthly := values["monthly-usd"]; {
		case monthly == "":
			note(client.CreditProblemMonthlyMissing)
		case !client.ValidMonthlyUSD(monthly):
			note(client.CreditProblemMonthlyInvalid)
		default:
			item.credit.MonthlyUSD = monthly
		}

		switch renews := values["renews"]; {
		case renews == "":
			note(client.CreditProblemRenewsMissing)
		default:
			if _, err := client.ParseRenewal(renews); err != nil {
				note(client.CreditProblemRenewsInvalid)
			} else {
				item.credit.Renews = renews
			}
		}

		item.credit.Problem = problem
		item.id = "item-" + strconv.Itoa(index+1)
		if item.credit.Label != "" {
			item.id = client.APICreditAccount(item.credit.Label)
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

// creditKeyFingerprint is the same rule as openRouterAccount: the first 12 hex
// digits of the key's SHA-256. It is what the snapshot holds in place of the
// key.
func creditKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "key-" + hex.EncodeToString(sum[:6])
}

// creditFailure names why an anthropic-api poll failed, in the fixed words of
// the client.CreditError values. Nothing Anthropic sent is kept: no body, no
// header, no request id, no message. A 429 is left as it is, for the capturing
// doer to turn into a rate limit that backs off only this organization, and a
// transport failure stays the generic "quota fetch failed".
func creditFailure(err error) error {
	var status quota.HTTPStatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &status):
		switch code := status.StatusCode; {
		case code == 429:
			return err
		case code == 401:
			return cache.PollError{Message: client.CreditErrorKeyRejected}
		case code == 403:
			return cache.PollError{Message: client.CreditErrorForbidden}
		case code == 404:
			return cache.PollError{Message: client.CreditErrorUnavailable}
		case code >= 500:
			return cache.PollError{Message: client.CreditErrorUpstream}
		default:
			return cache.PollError{Message: client.CreditErrorRefused}
		}
	case errors.Is(err, quota.ErrInvalidResponse):
		return cache.PollError{Message: client.CreditErrorResponse}
	}
	return err
}
