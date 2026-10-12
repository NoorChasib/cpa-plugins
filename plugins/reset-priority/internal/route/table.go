package route

import (
	"sort"
	"strings"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// Input is what one rebuild knows. Build reads nothing else.
type Input struct {
	Now          time.Time
	CreditsError string          // "", "missing", "unreadable"
	Credits      *qc.CreditState // already qc.CreditsAt(state, Now, MaxMeterAge); nil when CreditsError != ""
	MeterError   string          // "", "missing", "unreadable"
	Meter        *qc.APIMeter    // this rebuild's meter, nil when MeterError != ""
}

// Table is one rebuild's routing table. It is never mutated after Build
// returns, so Pick reads it through one atomic load with no lock.
type Table struct {
	BuiltAt, MeterFlushedAt, AuthIDsSince time.Time
	// ValidUntil is the credit state's valid_until, nil when it has none. From
	// then on the state's figures may be last cycle's, so Pick declines until
	// a build reads Quota Cache's next state.
	ValidUntil *time.Time
	Reason     Reason // ReasonNone when active
	// Orgs lists the linked organizations: the eligible ones by rank, then
	// the rest in the credit state's order.
	Orgs []Org
	// MappedKeys counts the keys linked to an organization in Orgs, and
	// FailingKeys those of them mapped to an eligible organization but not
	// proven. Each count covers only keys seen within RecentKeyAge; older
	// keys count only in StaleKeys, so a rotated key's old ID does not read
	// as a second key.
	MappedKeys, UnmappedKeys, FailingKeys int
	ForeignKeys, UnlinkedKeys, StaleKeys  int
	keys                                  map[string]key // auth ID → key
	// rechecks indexes the Orgs with Recheck, soonest refill first.
	rechecks []int16
}

// routes reports whether the table would route at now.
func (t *Table) routes(now time.Time) bool {
	return t.Reason == ReasonNone && now.Sub(t.BuiltAt) <= MaxTableAge && now.Sub(t.MeterFlushedAt) <= MaxMeterAge &&
		(t.ValidUntil == nil || now.Before(*t.ValidUntil))
}

type keyClass uint8

const (
	// classUnmapped is a key whose auth index the meter has not linked to an
	// organization, or no longer remembers.
	classUnmapped keyClass = iota
	// classForeign is a key whose responses never name an organization: not
	// Anthropic's.
	classForeign
	// classUnlinked is a key of an organization Quota Cache does not list.
	classUnlinked
	// classIneligible is a key of a linked organization that cannot be routed
	// to.
	classIneligible
	// classFailing is a key of an eligible organization that is not proven:
	// it has never succeeded, or has failed ProvenFailures times since it
	// last did. It is never the ranked choice, only explored.
	classFailing
	classRanked
)

type key struct {
	class keyClass
	// proven is set on a key of a linked organization, eligible or not, that
	// has succeeded and has not failed ProvenFailures times since. A re-check
	// of an ineligible organization prefers such a key, as the ranked choice
	// does.
	proven bool
	rank   int32 // the org's rank (≥ 1) for ranked and failing keys; 0 otherwise
	org    int16 // index into Orgs; −1 without a linked org
	seen   int64 // Meter.AuthIDs[id].SeenAt, unix seconds
}

// Org is one linked organization as the router sees it.
type Org struct {
	ID, Label                  string
	Rank                       int // 1-based among eligible, 0 otherwise
	Eligible, Demoted, Recheck bool
	Reason                     string
	RenewsAt                   *time.Time
	LeftText, EstimateLeftText string
	Refusal                    string // refused, refusedNearlySpent, claudeCodeRefused or ""
	Issue                      string // the account's issue sentence, as Quota Cache wrote it
	Keys, FailingKeys          int
}

// Org reasons, as the status spells them.
const (
	orgClaudeCodeRefused = "claude_code_refused"
	orgNoEstimate        = "no_estimate"
	orgNoCreditLeft      = "no_credit_left"
	orgOut               = "out"
	orgStale             = "stale"
	orgNeedsSettings     = "needs_settings"
	orgPending           = "pending"
	orgMisconfigured     = "misconfigured"
	orgCacheTooOld       = "cache_too_old"
	orgUnknownState      = "unknown_state"
)

// refusals are the account data issues an Org's Refusal can be, in no
// particular order: the first of them in the account's issues is the one
// shown.
var refusals = [...]string{qc.CreditIssueRefused, qc.CreditIssueRefusedNearlySpent, qc.CreditIssueClaudeCodeRefused}

// maxOrgs keeps an Orgs index inside key.org.
const maxOrgs = 1<<15 - 1

// Build turns one rebuild's input into a table. It is pure: the same input
// gives a deep-equal table, and it reads no clock and no file.
func Build(in Input) *Table {
	t := &Table{BuiltAt: in.Now}
	if in.Meter != nil {
		t.MeterFlushedAt = in.Meter.FlushedAt
		if in.Meter.AuthIDsSince != nil {
			t.AuthIDsSince = *in.Meter.AuthIDsSince
		}
	}
	if in.Credits != nil && in.Credits.ValidUntil != nil {
		until := *in.Credits.ValidUntil
		t.ValidUntil = &until
	}
	if t.Reason = inactive(in); t.Reason != ReasonNone {
		return t
	}
	byOrg := t.orgs(in.Credits.APICredits.Accounts)
	t.mapKeys(in, byOrg)
	return t
}

// inactive is the first reason the input cannot route, in order, or
// ReasonNone.
func inactive(in Input) Reason {
	switch {
	case in.CreditsError == "unreadable":
		return ReasonCreditsUnreadable
	case in.Credits == nil || in.CreditsError != "":
		// No credit state file: Quota Cache older than 0.2.0, or one that
		// has never published.
		return ReasonCreditsMissing
	case !in.Credits.Current(in.Now, MaxCreditStateAge):
		// Quota Cache publishes at least every minute while it runs, and
		// again at valid_until. A state older than MaxCreditStateAge, or one
		// past its valid_until, is one it no longer keeps current: disabled,
		// stopped or wedged.
		return ReasonCreditsStale
	case in.Credits.Settings.State != qc.CreditSettingsOK:
		// Quota Cache cannot apply the operator's saved values: it cannot
		// read its settings file or, before it has one, Quota Glance's
		// settings.json. Its figures come from the configuration alone and
		// could be wrong.
		return ReasonSettingsUnavailable
	case in.MeterError == "unreadable":
		return ReasonMeterUnreadable
	case in.Meter == nil || in.MeterError != "":
		return ReasonMeterMissing
	case in.Meter.StoppedAt != nil:
		// Ahead of the auth IDs: quota-cache adds AuthIDsSince only when its
		// meter starts, so a file an older quota-cache left stopped still has
		// none after the update, and starting the meter is what fixes it.
		return ReasonMeterStopped
	case in.Meter.AuthIDsSince == nil:
		// A meter written by quota-cache older than 0.2.0 records no auth
		// IDs, so no candidate can be linked to an organization.
		return ReasonMeterTooOld
	case in.Now.Sub(in.Meter.FlushedAt) > MaxMeterAge:
		return ReasonMeterStale
	case in.Credits.APICredits == nil:
		// Quota Cache lists no Claude Console organization.
		return ReasonNoAPICredits
	}
	for i := range in.Credits.APICredits.Accounts {
		if in.Credits.APICredits.Accounts[i].OrganizationID != "" {
			return ReasonNone
		}
	}
	return ReasonNoLinkedOrgs
}

// orgs fills t.Orgs and t.rechecks from the credit state's accounts, and
// returns each linked organization ID's index into t.Orgs.
func (t *Table) orgs(accounts []qc.CreditAccount) map[string]int16 {
	// One account per organization: the first whose ID is org-<hex>, else
	// the first. The others are duplicates or over-limit items, which Quota
	// Cache lists as item-<n> and does not count.
	first := map[string]int{}
	kept := []int{}
	for i := range accounts {
		org := strings.ToLower(accounts[i].OrganizationID)
		if org == "" {
			continue
		}
		j, seen := first[org]
		switch {
		case !seen:
			if len(kept) == maxOrgs {
				continue
			}
			first[org] = len(kept)
			kept = append(kept, i)
		case !isOrgAccount(accounts[kept[j]].ID) && isOrgAccount(accounts[i].ID):
			kept[j] = i
		}
	}
	sort.Ints(kept)

	var eligible, rest []linkedOrg
	for _, i := range kept {
		a := &accounts[i]
		org := linkedOrg{Org: orgOf(a), org: strings.ToLower(a.OrganizationID), order: a.Order}
		if org.Eligible {
			eligible = append(eligible, org)
		} else {
			rest = append(rest, org)
		}
	}
	// Demoted organizations after every clean one, then the soonest refill,
	// with no date after every known one, then the configured order, then
	// the account ID, which is unique.
	sort.SliceStable(eligible, func(a, b int) bool {
		if eligible[a].Demoted != eligible[b].Demoted {
			return !eligible[a].Demoted
		}
		return eligible[a].refillsBefore(&eligible[b])
	})
	all := append(eligible, rest...)

	t.Orgs = make([]Org, len(all))
	byOrg := make(map[string]int16, len(all))
	for i := range all {
		if all[i].Eligible {
			all[i].Rank = i + 1
		}
		t.Orgs[i] = all[i].Org
		byOrg[all[i].org] = int16(i)
		if all[i].Recheck {
			t.rechecks = append(t.rechecks, int16(i))
		}
	}
	sort.SliceStable(t.rechecks, func(a, b int) bool {
		return all[t.rechecks[a]].refillsBefore(&all[t.rechecks[b]])
	})
	return byOrg
}

// linkedOrg is an Org while Build ranks it.
type linkedOrg struct {
	Org
	org   string // the lower-cased organization ID
	order int    // the account's configured position
}

// refillsBefore orders by refill, with no date after every known one, then by
// the configured order, then by account ID.
func (x *linkedOrg) refillsBefore(y *linkedOrg) bool {
	switch {
	case x.RenewsAt != nil && y.RenewsAt == nil:
		return true
	case x.RenewsAt == nil && y.RenewsAt != nil:
		return false
	case x.RenewsAt != nil && !x.RenewsAt.Equal(*y.RenewsAt):
		return x.RenewsAt.Before(*y.RenewsAt)
	case x.order != y.order:
		return x.order < y.order
	}
	return x.ID < y.ID
}

func isOrgAccount(id string) bool { return strings.HasPrefix(id, "org-") }

// orgOf is one kept account as an Org, with its eligibility.
func orgOf(a *qc.CreditAccount) Org {
	org := Org{
		ID: a.ID, Label: a.Label, LeftText: a.LeftText, EstimateLeftText: a.EstimateLeftText,
		Refusal: refusalOf(a.DataIssues), Issue: a.Issue,
	}
	if a.RenewsAtEpoch != nil {
		at := time.Unix(*a.RenewsAtEpoch, 0).UTC()
		org.RenewsAt = &at
	}
	switch a.State {
	case qc.CreditStateOK:
		switch {
		case !a.HasEstimate:
			org.Reason = orgNoEstimate
		case a.Left <= 0:
			// Never re-checked: a success there could be billed to purchased
			// credit. Within the cycle only a change to the account's settings,
			// such as a new Console reading, clears it; the next refill clears
			// it on its own, since Left is then the new cycle's.
			org.Reason = orgNoCreditLeft
		case hasIssue(a.DataIssues, qc.CreditIssueClaudeCodeRefused):
			// A refusal in a Claude Code-based session while the estimate is
			// above 10%: Claude Code, which the credit does not cover, or the
			// Agent SDK, which it does. Ranked after every clean organization
			// and re-checked, rather than trusted or written off.
			org.Eligible, org.Demoted, org.Recheck, org.Reason = true, true, true, orgClaudeCodeRefused
		default:
			org.Eligible = true
		}
	case qc.CreditStateOut:
		org.Reason = orgOut
		// out clears only after a later success, which routing would
		// withhold while any other organization is eligible. An estimate that
		// still had credit left is re-checked, so a false or premature out
		// cannot strand the rest of the cycle's credit.
		org.Recheck = a.EstimateLeftText != "" && a.EstimateLeftText != "$0.00"
	case qc.CreditStateStale:
		org.Reason = orgStale
	case qc.CreditStateNeedsSettings:
		org.Reason = orgNeedsSettings
	case qc.CreditStatePending:
		org.Reason = orgPending
	case qc.CreditStateMisconfigured:
		org.Reason = orgMisconfigured
	case qc.CreditStateCacheTooOld:
		org.Reason = orgCacheTooOld
	default:
		org.Reason = orgUnknownState
	}
	return org
}

// refusalOf is the account's refusal data issue, the first in the account's
// order, or "".
func refusalOf(issues []string) string {
	for _, issue := range issues {
		for _, refusal := range refusals {
			if issue == refusal {
				return issue
			}
		}
	}
	return ""
}

func hasIssue(issues []string, want string) bool {
	for _, issue := range issues {
		if issue == want {
			return true
		}
	}
	return false
}

// mapKeys classifies every auth ID the meter recorded: auth ID → auth index
// (auth_ids) → organization (auths) → that organization's Org.
func (t *Table) mapKeys(in Input, byOrg map[string]int16) {
	t.keys = make(map[string]key, len(in.Meter.AuthIDs))
	for id, e := range in.Meter.AuthIDs {
		if !qc.ConfigAPIKeyAuthID(id) {
			continue
		}
		// An unmapped key keeps its SeenAt: exploration reads it to tell
		// whether an explored attempt produced a usage record.
		k := key{class: classUnmapped, org: -1, seen: e.SeenAt.Unix()}
		if a, ok := in.Meter.Auths[e.AuthIndex]; ok {
			org := strings.ToLower(a.OrganizationID)
			j, linked := byOrg[org]
			// Proven: it has succeeded, and has not failed twice since. A key
			// that fails every request with a 400 CPA does not fail over (a
			// missing workspace ID, a spend limit) never is, so it is never
			// ranked first, nor the key a re-check prefers; one client-caused
			// 400 between successes does not demote a key.
			proven := e.LastSuccessAt != nil && e.Failures < ProvenFailures
			switch {
			case org == "":
				k.class = classForeign
			case !linked:
				k.class = classUnlinked
			case !t.Orgs[j].Eligible:
				k.class, k.org, k.proven = classIneligible, j, proven
			default:
				k.class, k.org, k.rank, k.proven = classFailing, j, int32(t.Orgs[j].Rank), proven
				if proven {
					k.class = classRanked
				}
			}
		}
		t.keys[id] = k

		if in.Now.Sub(e.SeenAt) > RecentKeyAge {
			t.StaleKeys++
			continue
		}
		switch k.class {
		case classUnmapped:
			t.UnmappedKeys++
		case classForeign:
			t.ForeignKeys++
		case classUnlinked:
			t.UnlinkedKeys++
		default:
			t.MappedKeys++
			t.Orgs[k.org].Keys++
			if k.class == classFailing {
				t.FailingKeys++
				t.Orgs[k.org].FailingKeys++
			}
		}
	}
}
