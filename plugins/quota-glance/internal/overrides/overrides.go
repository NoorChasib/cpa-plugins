// Package overrides keeps the values an operator sets from the dashboard: an
// API credit's monthly amount and refill date, a Console reading of what is
// left, and a Claude subscription's renewal date. They live in
// <data-dir>/settings.json and win over quota-cache's configuration, field by
// field.
//
// Loading checks shape only. A value that was valid when it was saved stays
// loadable however old it gets: a reading taken before the last refill, or for
// an organization the configuration no longer names, is still a well-formed
// reading, and the document reports it as unused rather than this package
// refusing the whole file. The time rules apply when a value is saved.
//
// A file that fails a shape rule is never overwritten. Nothing in it applies,
// editing is unavailable, and health says why, until the operator moves it
// aside.
package overrides

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

const (
	// Schema is the only settings.json schema this build reads or writes.
	Schema = 1
	// FileName is settings.json's name inside data-dir.
	FileName = "settings.json"
	// The file's bounds. A save past one first evicts renewal dates for
	// credentials the dashboard no longer lists, and is refused when that is
	// not enough.
	MaxAPICredits = 64
	MaxRenewals   = 256
	MaxBytes      = 1 << 20
	// MaxBaseline bounds a reading's baseline. Usage past it is summed under
	// qc.MeterOtherModel, which is never priced: the baseline then reads low,
	// and the spend since the reading high, the safe direction.
	MaxBaseline = qc.MaxMeterUsagePerBucket
)

// stampLayout is every instant in the file: RFC 3339 in UTC, with "Z", in
// whole seconds.
const stampLayout = "2006-01-02T15:04:05Z"

// Values is one committed state of settings.json. It is never modified once a
// Store hands it out; a save swaps in a new one.
type Values struct {
	// Revision counts committed writes. Each entry's Rev is the revision that
	// last wrote it.
	Revision uint64
	// APICredits is keyed by the anthropic-api account id, "org-" and 12 hex
	// digits.
	APICredits map[string]APICredit
	// Renewals is keyed by a Claude credential's CPA auth index, 16 hex
	// digits.
	Renewals map[string]Renewal
	// Unreadable is set when settings.json exists but failed a shape rule.
	// Nothing in it applies, the maps are empty, and nothing may be saved
	// until the file is moved aside.
	Unreadable bool
}

// APICredit is the dashboard's values for one Console organization. An empty
// string is a value not set here, which leaves quota-cache's configured one in
// force.
type APICredit struct {
	MonthlyUSD string
	// Renews is a date, "2026-10-29".
	Renews    string
	Reading   *Reading
	Rev       uint64
	UpdatedAt time.Time
}

// Reading is what Console showed as left of the credit, and when.
type Reading struct {
	RemainingUSD string
	At           time.Time
	EnteredAt    time.Time
	// OrganizationID is the organization the account named when the reading
	// was saved. A reading for another organization is never used.
	OrganizationID string
	// Baseline is the meter's usage from 00:00 UTC of the reading's day to the
	// start of its hour, taken when it was saved. What the organization spent
	// since the reading is its daily usage from that day, less this.
	Baseline Baseline
}

// Baseline is a reading's token baseline. Usage is in the meter's own form, so
// it is priced by the same function as the meter it is subtracted from.
type Baseline struct {
	DayStart time.Time
	Until    time.Time
	Usage    []qc.MeterUsage
}

// Renewal is a Claude subscription's renewal date as the operator set it.
type Renewal struct {
	Date      string
	Rev       uint64
	UpdatedAt time.Time
}

// RevisionText is a revision as the wire carries it: decimal, and "" for
// nothing stored.
func RevisionText(rev uint64) string {
	if rev == 0 {
		return ""
	}
	return strconv.FormatUint(rev, 10)
}

var (
	apiCreditKey = regexp.MustCompile(`^org-[0-9a-f]{12}$`)
	renewalKey   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidAPICreditID reports whether id can key an API credit entry.
func ValidAPICreditID(id string) bool { return apiCreditKey.MatchString(id) }

// ValidRenewalID reports whether id can key a renewal date: a CPA auth index,
// which is 16 lower-case hex digits.
func ValidRenewalID(id string) bool { return renewalKey.MatchString(id) }

// ParseDate reads a date the way both renews and a renewal date are written:
// YYYY-MM-DD only, a real date in 2000 to 2099, at 00:00 UTC.
func ParseDate(s string) (time.Time, bool) {
	day, err := time.Parse(time.DateOnly, s)
	if err != nil || day.Format(time.DateOnly) != s || day.Year() < 2000 || day.Year() > 2099 {
		return time.Time{}, false
	}
	return day, true
}

// parseStamp reads an instant in the file's one form.
func parseStamp(s string) (time.Time, bool) {
	t, err := time.Parse(stampLayout, s)
	// Parse accepts a fractional second the layout does not name; the round
	// trip refuses it.
	if err != nil || t.Format(stampLayout) != s {
		return time.Time{}, false
	}
	return t, true
}

func stampOf(t time.Time) string { return t.UTC().Format(stampLayout) }

// The file as JSON. Every optional key is a pointer so that a present key is
// told from an absent one, which the shape rules care about.
type fileDoc struct {
	Schema     *int                     `json:"schema"`
	Revision   *uint64                  `json:"revision"`
	APICredits map[string]fileAPICredit `json:"apiCredits"`
	Renewals   map[string]fileRenewal   `json:"renewals"`
}

type fileAPICredit struct {
	MonthlyUSD *string      `json:"monthlyUsd,omitempty"`
	Renews     *string      `json:"renews,omitempty"`
	Reading    *fileReading `json:"reading,omitempty"`
	Rev        *uint64      `json:"rev"`
	UpdatedAt  *string      `json:"updatedAt"`
}

type fileReading struct {
	RemainingUSD   *string       `json:"remainingUsd"`
	At             *string       `json:"at"`
	EnteredAt      *string       `json:"enteredAt"`
	OrganizationID *string       `json:"organizationId"`
	Baseline       *fileBaseline `json:"baseline"`
}

type fileBaseline struct {
	DayStart *string         `json:"dayStart"`
	Until    *string         `json:"until"`
	Usage    []qc.MeterUsage `json:"usage"`
}

type fileRenewal struct {
	Date      *string `json:"date"`
	Rev       *uint64 `json:"rev"`
	UpdatedAt *string `json:"updatedAt"`
}

// errShape is every load failure. Which rule failed is not reported anywhere:
// health says the file is unreadable, and the operator's remedy is the same.
var errShape = errors.New("settings.json fails a shape rule")

// decode reads settings.json and applies the load-time shape rules. An entry
// with no value at all is dropped, as a save would have deleted it.
func decode(raw []byte) (Values, error) {
	if len(raw) > MaxBytes {
		return Values{}, errShape
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc fileDoc
	if decoder.Decode(&doc) != nil || decoder.More() {
		return Values{}, errShape
	}
	if doc.Schema == nil || *doc.Schema != Schema || doc.Revision == nil ||
		len(doc.APICredits) > MaxAPICredits || len(doc.Renewals) > MaxRenewals {
		return Values{}, errShape
	}
	v := Values{
		Revision:   *doc.Revision,
		APICredits: make(map[string]APICredit, len(doc.APICredits)),
		Renewals:   make(map[string]Renewal, len(doc.Renewals)),
	}
	validRev := func(rev *uint64) bool { return rev != nil && *rev >= 1 && *rev <= v.Revision }
	for id, entry := range doc.APICredits {
		if !ValidAPICreditID(id) || !validRev(entry.Rev) || entry.UpdatedAt == nil {
			return Values{}, errShape
		}
		updated, ok := parseStamp(*entry.UpdatedAt)
		if !ok {
			return Values{}, errShape
		}
		credit := APICredit{Rev: *entry.Rev, UpdatedAt: updated}
		if entry.MonthlyUSD != nil {
			if !qc.ValidMonthlyUSD(*entry.MonthlyUSD) {
				return Values{}, errShape
			}
			credit.MonthlyUSD = *entry.MonthlyUSD
		}
		if entry.Renews != nil {
			if _, ok := ParseDate(*entry.Renews); !ok {
				return Values{}, errShape
			}
			credit.Renews = *entry.Renews
		}
		if entry.Reading != nil {
			reading, ok := decodeReading(*entry.Reading)
			if !ok {
				return Values{}, errShape
			}
			credit.Reading = reading
		}
		if credit.MonthlyUSD != "" || credit.Renews != "" || credit.Reading != nil {
			v.APICredits[id] = credit
		}
	}
	for id, entry := range doc.Renewals {
		if !ValidRenewalID(id) || !validRev(entry.Rev) || entry.UpdatedAt == nil {
			return Values{}, errShape
		}
		updated, ok := parseStamp(*entry.UpdatedAt)
		if !ok {
			return Values{}, errShape
		}
		if entry.Date == nil {
			continue
		}
		if _, ok := ParseDate(*entry.Date); !ok {
			return Values{}, errShape
		}
		v.Renewals[id] = Renewal{Date: *entry.Date, Rev: *entry.Rev, UpdatedAt: updated}
	}
	return v, nil
}

func decodeReading(r fileReading) (*Reading, bool) {
	if r.RemainingUSD == nil || r.At == nil || r.EnteredAt == nil || r.OrganizationID == nil || r.Baseline == nil ||
		r.Baseline.DayStart == nil || r.Baseline.Until == nil {
		return nil, false
	}
	if !qc.ValidMonthlyUSD(*r.RemainingUSD) {
		return nil, false
	}
	at, okAt := parseStamp(*r.At)
	entered, okEntered := parseStamp(*r.EnteredAt)
	dayStart, okDay := parseStamp(*r.Baseline.DayStart)
	until, okUntil := parseStamp(*r.Baseline.Until)
	if !okAt || !okEntered || !okDay || !okUntil {
		return nil, false
	}
	if org, ok := qc.NormalizeOrganizationID(*r.OrganizationID); !ok || org != *r.OrganizationID {
		return nil, false
	}
	if !dayStart.Equal(qc.MeterDayStart(at)) || !until.Equal(qc.MeterHourStart(at)) || len(r.Baseline.Usage) > MaxBaseline {
		return nil, false
	}
	usage := make([]qc.MeterUsage, 0, len(r.Baseline.Usage))
	for _, u := range r.Baseline.Usage {
		if u.Model != qc.MeterOtherModel && qc.NormalizeMeterModel(u.Model, "") != u.Model {
			return nil, false
		}
		if u.Prompt != "" && u.Prompt != qc.MeterPromptOver100K {
			return nil, false
		}
		usage = qc.MergeMeterUsage(usage, u)
	}
	return &Reading{
		RemainingUSD:   *r.RemainingUSD,
		At:             at,
		EnteredAt:      entered,
		OrganizationID: *r.OrganizationID,
		Baseline:       Baseline{DayStart: dayStart, Until: until, Usage: usage},
	}, true
}

// encode writes v as settings.json.
func encode(v Values) ([]byte, error) {
	revision, schema := v.Revision, Schema
	doc := fileDoc{
		Schema:     &schema,
		Revision:   &revision,
		APICredits: make(map[string]fileAPICredit, len(v.APICredits)),
		Renewals:   make(map[string]fileRenewal, len(v.Renewals)),
	}
	text := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	for id, credit := range v.APICredits {
		rev, updated := credit.Rev, stampOf(credit.UpdatedAt)
		entry := fileAPICredit{MonthlyUSD: text(credit.MonthlyUSD), Renews: text(credit.Renews), Rev: &rev, UpdatedAt: &updated}
		if r := credit.Reading; r != nil {
			remaining, at, entered, org := r.RemainingUSD, stampOf(r.At), stampOf(r.EnteredAt), r.OrganizationID
			dayStart, until := stampOf(r.Baseline.DayStart), stampOf(r.Baseline.Until)
			usage := r.Baseline.Usage
			if usage == nil {
				usage = []qc.MeterUsage{}
			}
			entry.Reading = &fileReading{
				RemainingUSD: &remaining, At: &at, EnteredAt: &entered, OrganizationID: &org,
				Baseline: &fileBaseline{DayStart: &dayStart, Until: &until, Usage: usage},
			}
		}
		doc.APICredits[id] = entry
	}
	for id, renewal := range v.Renewals {
		date, rev, updated := renewal.Date, renewal.Rev, stampOf(renewal.UpdatedAt)
		doc.Renewals[id] = fileRenewal{Date: &date, Rev: &rev, UpdatedAt: &updated}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// clone copies the maps, so a save can build the next state without touching
// the one readers hold. Entries are values, and a Reading is never modified
// once stored, so sharing it is safe.
func (v Values) clone() Values {
	next := Values{
		Revision:   v.Revision,
		APICredits: make(map[string]APICredit, len(v.APICredits)),
		Renewals:   make(map[string]Renewal, len(v.Renewals)),
	}
	for id, credit := range v.APICredits {
		next.APICredits[id] = credit
	}
	for id, renewal := range v.Renewals {
		next.Renewals[id] = renewal
	}
	return next
}

func empty() Values {
	return Values{APICredits: map[string]APICredit{}, Renewals: map[string]Renewal{}}
}
