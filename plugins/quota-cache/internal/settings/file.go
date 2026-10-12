package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// FilePath is where the settings live for the snapshot at snapshotPath: the
// same path with a final ".json" replaced by ".settings.json", else with
// ".settings.json" appended. It is operator data, not cache, and must move
// with the snapshot.
func FilePath(snapshotPath string) string {
	return strings.TrimSuffix(snapshotPath, ".json") + ".settings.json"
}

// stampLayout is every instant in the file: RFC 3339 in UTC, with "Z", in
// whole seconds.
const stampLayout = "2006-01-02T15:04:05Z"

var apiCreditKey = regexp.MustCompile(`^org-[0-9a-f]{12}$`)

// ValidAPICreditID reports whether id can key an API credit entry.
func ValidAPICreditID(id string) bool { return apiCreditKey.MatchString(id) }

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
	DayStart *string             `json:"dayStart"`
	Until    *string             `json:"until"`
	Usage    []client.MeterUsage `json:"usage"`
}

type fileRenewal struct {
	Date      *string `json:"date"`
	Rev       *uint64 `json:"rev"`
	UpdatedAt *string `json:"updatedAt"`
}

// errShape is every load failure. Which rule failed is not reported anywhere:
// the credit state says the file is unreadable, and the operator's remedy is
// the same.
var errShape = errors.New("settings file fails a shape rule")

// decode reads the settings file and applies the load-time shape rules. An
// entry with no value at all is dropped, as a save would have deleted it.
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
			if !client.ValidMonthlyUSD(*entry.MonthlyUSD) {
				return Values{}, errShape
			}
			credit.MonthlyUSD = *entry.MonthlyUSD
		}
		if entry.Renews != nil {
			if _, ok := client.ParseRenewalDate(*entry.Renews); !ok {
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
		if !client.ValidRenewalID(id) || !validRev(entry.Rev) || entry.UpdatedAt == nil {
			return Values{}, errShape
		}
		updated, ok := parseStamp(*entry.UpdatedAt)
		if !ok {
			return Values{}, errShape
		}
		if entry.Date == nil {
			continue
		}
		if _, ok := client.ParseRenewalDate(*entry.Date); !ok {
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
	if !client.ValidMonthlyUSD(*r.RemainingUSD) {
		return nil, false
	}
	at, okAt := parseStamp(*r.At)
	entered, okEntered := parseStamp(*r.EnteredAt)
	dayStart, okDay := parseStamp(*r.Baseline.DayStart)
	until, okUntil := parseStamp(*r.Baseline.Until)
	if !okAt || !okEntered || !okDay || !okUntil {
		return nil, false
	}
	if org, ok := client.NormalizeOrganizationID(*r.OrganizationID); !ok || org != *r.OrganizationID {
		return nil, false
	}
	if !dayStart.Equal(client.MeterDayStart(at)) || !until.Equal(client.MeterHourStart(at)) || len(r.Baseline.Usage) > MaxBaseline {
		return nil, false
	}
	usage := make([]client.MeterUsage, 0, len(r.Baseline.Usage))
	for _, u := range r.Baseline.Usage {
		if u.Model != client.MeterOtherModel && client.NormalizeMeterModel(u.Model, "") != u.Model {
			return nil, false
		}
		if u.Prompt != "" && u.Prompt != client.MeterPromptOver100K {
			return nil, false
		}
		usage = client.MergeMeterUsage(usage, u)
	}
	return &Reading{
		RemainingUSD:   *r.RemainingUSD,
		At:             at,
		EnteredAt:      entered,
		OrganizationID: *r.OrganizationID,
		Baseline:       Baseline{DayStart: dayStart, Until: until, Usage: usage},
	}, true
}

// encode writes v as the settings file, in exactly the bytes Quota Glance's
// encoder writes for the same values.
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
				usage = []client.MeterUsage{}
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

func unreadable() Values {
	v := empty()
	v.Unreadable = true
	return v
}
