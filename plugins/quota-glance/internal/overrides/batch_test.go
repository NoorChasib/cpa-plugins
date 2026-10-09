package overrides

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseBatchReadsBothKinds(t *testing.T) {
	b, err := ParseBatch([]byte(`{"kind":"apiCredits","items":[
		{"id":"org-3f2a9c1d0b7e","baseRevision":"7","monthlyUsd":"260.50","renews":null,
		 "reading":{"remainingUsd":"143.20","at":"2026-10-09T13:20:00Z"}},
		{"id":"org-000000000001","baseRevision":"","monthlyUsd":null,"renews":"2026-10-29","reading":null}]}`))
	if err != nil || b.Kind != KindAPICredits || len(b.APICredits) != 2 {
		t.Fatalf("%+v %v", b, err)
	}
	first, second := b.APICredits[0], b.APICredits[1]
	if first.ID != "org-3f2a9c1d0b7e" || first.BaseRevision != "7" || *first.MonthlyUSD != "260.50" || first.Renews != nil ||
		*first.Reading != (ReadingInput{RemainingUSD: "143.20", At: "2026-10-09T13:20:00Z"}) ||
		second.MonthlyUSD != nil || *second.Renews != "2026-10-29" || second.Reading != nil || second.Clears() {
		t.Fatalf("items = %+v", b.APICredits)
	}
	b, err = ParseBatch([]byte(`{"kind":"renewals","items":[{"id":"0123456789abcdef","baseRevision":"","date":"2026-10-29"},{"id":"fedcba9876543210","baseRevision":"3","date":null}]}`))
	if err != nil || len(b.Renewals) != 2 || *b.Renewals[0].Date != "2026-10-29" || !b.Renewals[1].Clears() {
		t.Fatalf("%+v %v", b, err)
	}
	if ids := b.IDs(); len(ids) != 2 || ids[1] != "fedcba9876543210" {
		t.Fatalf("ids = %v", ids)
	}
}

// Everything that is not exactly a batch is invalid_request, before any value
// is looked at.
func TestParseBatchRefusesEverythingElse(t *testing.T) {
	item := `{"id":"org-3f2a9c1d0b7e","baseRevision":"","monthlyUsd":"1","renews":null,"reading":null}`
	many := func(n int, kind string) string {
		items := make([]string, n)
		for i := range items {
			if kind == KindAPICredits {
				items[i] = strings.Replace(item, "3f2a9c1d0b7e", strings.Repeat("0", 10)+string(rune('a'+i/26))+string(rune('a'+i%26)), 1)
			} else {
				items[i] = `{"id":"` + strings.Repeat("0", 14) + string(rune('a'+i/26)) + string(rune('a'+i%26)) + `","baseRevision":"","date":null}`
			}
		}
		return `{"kind":"` + kind + `","items":[` + strings.Join(items, ",") + `]}`
	}
	for name, body := range map[string]string{
		"empty":                  ``,
		"null":                   `null`,
		"an array":               `[` + item + `]`,
		"a string":               `"apiCredits"`,
		"no kind":                `{"items":[` + item + `]}`,
		"null kind":              `{"kind":null,"items":[` + item + `]}`,
		"unknown kind":           `{"kind":"budgets","items":[` + item + `]}`,
		"no items":               `{"kind":"apiCredits"}`,
		"zero items":             `{"kind":"apiCredits","items":[]}`,
		"seventeen items":        many(17, KindAPICredits),
		"thirty-three renewals":  many(33, KindRenewals),
		"an unknown top key":     `{"kind":"apiCredits","items":[` + item + `],"force":true}`,
		"an unknown item key":    `{"kind":"apiCredits","items":[` + strings.Replace(item, `"reading":null`, `"reading":null,"note":"x"`, 1) + `]}`,
		"a missing value key":    `{"kind":"apiCredits","items":[` + strings.Replace(item, `,"reading":null`, ``, 1) + `]}`,
		"a missing id":           `{"kind":"apiCredits","items":[` + strings.Replace(item, `"id":"org-3f2a9c1d0b7e",`, ``, 1) + `]}`,
		"a null id":              `{"kind":"apiCredits","items":[` + strings.Replace(item, `"org-3f2a9c1d0b7e"`, `null`, 1) + `]}`,
		"a missing base":         `{"kind":"apiCredits","items":[` + strings.Replace(item, `"baseRevision":"",`, ``, 1) + `]}`,
		"a numeric base":         `{"kind":"apiCredits","items":[` + strings.Replace(item, `"baseRevision":""`, `"baseRevision":7`, 1) + `]}`,
		"a malformed base":       `{"kind":"apiCredits","items":[` + strings.Replace(item, `"baseRevision":""`, `"baseRevision":"07"`, 1) + `]}`,
		"a repeated id":          `{"kind":"apiCredits","items":[` + item + `,` + item + `]}`,
		"a numeric amount":       `{"kind":"apiCredits","items":[` + strings.Replace(item, `"monthlyUsd":"1"`, `"monthlyUsd":1`, 1) + `]}`,
		"a reading missing at":   `{"kind":"apiCredits","items":[` + strings.Replace(item, `"reading":null`, `"reading":{"remainingUsd":"1"}`, 1) + `]}`,
		"a reading with more":    `{"kind":"apiCredits","items":[` + strings.Replace(item, `"reading":null`, `"reading":{"remainingUsd":"1","at":"x","by":"me"}`, 1) + `]}`,
		"a renewal's item":       `{"kind":"renewals","items":[` + item + `]}`,
		"a renewal missing date": `{"kind":"renewals","items":[{"id":"0123456789abcdef","baseRevision":""}]}`,
		"trailing data":          `{"kind":"apiCredits","items":[` + item + `]} {}`,
	} {
		if _, err := ParseBatch([]byte(body)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseBatch([]byte(many(16, KindAPICredits))); err != nil {
		t.Errorf("sixteen rows: %v", err)
	}
	if _, err := ParseBatch([]byte(many(32, KindRenewals))); err != nil {
		t.Errorf("thirty-two renewals: %v", err)
	}
}

// The shared vectors the page tests too, as the server judges them: what the
// page sends is accepted, and what it never sends is refused with its code.
func TestCheckSharedVectors(t *testing.T) {
	now := at(t, "2026-10-09T14:00:00Z")
	check := func(item APICreditItem) *FieldError {
		return Batch{APICredits: []APICreditItem{item}}.Check(Values{}, now)
	}
	for _, sent := range []string{"200", "260.5", "260.50", "1200.50", "1200", "0", "9999999.99"} {
		if fail := check(APICreditItem{ID: accountA, MonthlyUSD: ptr(sent)}); fail != nil {
			t.Errorf("%q: %v", sent, fail)
		}
	}
	for _, sent := range []string{"200.123", "-1", "1e3", "12345678", ".5", "5.", "$200", " 200 ", "1,200", "12,50", ""} {
		if fail := check(APICreditItem{ID: accountA, MonthlyUSD: ptr(sent)}); fail == nil || *fail != (FieldError{Code: CodeMonthlyUSD, ID: accountA, Field: "monthlyUsd"}) {
			t.Errorf("%q: %v", sent, fail)
		}
	}
	for _, sent := range []string{"2026-02-30", "2026-11-31", "2026-10-29T00:00:00Z", "1999-12-31", "2100-01-01", "26-10-29", "2026-1-9"} {
		if fail := check(APICreditItem{ID: accountA, Renews: ptr(sent)}); fail == nil || fail.Code != CodeRenews || fail.Field != "renews" {
			t.Errorf("renews %q: %v", sent, fail)
		}
		if fail := (Batch{Renewals: []RenewalItem{{ID: renewalA, Date: ptr(sent)}}}).Check(Values{}, now); fail == nil || *fail != (FieldError{Code: CodeDate, ID: renewalA, Field: "date"}) {
			t.Errorf("date %q: %v", sent, fail)
		}
	}
	if fail := check(APICreditItem{ID: accountA, Renews: ptr("2028-02-29")}); fail != nil {
		t.Errorf("a leap day: %v", fail)
	}
}

// A new reading must be from the last 48 hours by the server's clock, no more
// than five minutes ahead, and from this cycle; an unchanged one is not
// checked against the clock again.
func TestCheckReadingRules(t *testing.T) {
	now := at(t, "2026-10-09T14:00:00Z")
	reading := func(amount, when string) *ReadingInput { return &ReadingInput{RemainingUSD: amount, At: when} }
	for name, tc := range map[string]struct {
		item APICreditItem
		code string
	}{
		"now":                      {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-09T14:00:00Z")}, ""},
		"48 hours ago":             {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-07T14:00:00Z")}, ""},
		"48 hours and a second":    {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-07T13:59:59Z")}, CodeReadingTime},
		"five minutes ahead":       {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-09T14:05:00Z")}, ""},
		"more than five ahead":     {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-09T14:05:01Z")}, CodeReadingTime},
		"with an offset":           {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-09T13:00:00+00:00")}, CodeReadingTime},
		"with a fraction":          {APICreditItem{ID: accountA, Reading: reading("1.00", "2026-10-09T13:00:00.5Z")}, CodeReadingTime},
		"not a time":               {APICreditItem{ID: accountA, Reading: reading("1.00", "today")}, CodeReadingTime},
		"a bad amount":             {APICreditItem{ID: accountA, Reading: reading("1.001", "2026-10-09T13:00:00Z")}, CodeReadingAmount},
		"before the config refill": {APICreditItem{ID: accountA, ConfigRenews: "2026-10-09", Reading: reading("1.00", "2026-10-08T23:59:59Z")}, CodeReadingBeforeRefill},
		"after the config refill":  {APICreditItem{ID: accountA, ConfigRenews: "2026-10-09", Reading: reading("1.00", "2026-10-09T00:00:00Z")}, ""},
		// The batch's own refill date is the one that counts, and "Use config"
		// (renews null) brings the config's back.
		"before the batch's refill":  {APICreditItem{ID: accountA, Renews: ptr("2026-10-09"), Reading: reading("1.00", "2026-10-08T12:00:00Z")}, CodeReadingBeforeRefill},
		"the batch moves the refill": {APICreditItem{ID: accountA, ConfigRenews: "2026-10-09", Renews: ptr("2026-10-01"), Reading: reading("1.00", "2026-10-08T12:00:00Z")}, ""},
	} {
		fail := Batch{APICredits: []APICreditItem{tc.item}}.Check(Values{}, now)
		switch {
		case tc.code == "" && fail != nil:
			t.Errorf("%s: %v", name, fail)
		case tc.code != "" && (fail == nil || fail.Code != tc.code || fail.ID != accountA):
			t.Errorf("%s: %v, want %s", name, fail, tc.code)
		case fail != nil && fail.Code == CodeReadingAmount && fail.Field != "reading.remainingUsd":
			t.Errorf("%s: field %s", name, fail.Field)
		case fail != nil && fail.Code != CodeReadingAmount && fail.Field != "reading.at":
			t.Errorf("%s: field %s", name, fail.Field)
		}
	}
	// A week-old reading resent unchanged with a new credit saves.
	stored := Values{APICredits: map[string]APICredit{accountA: {Rev: 1, Reading: &Reading{RemainingUSD: "150.00", At: now.Add(-7 * 24 * time.Hour)}}}}
	unchanged := APICreditItem{ID: accountA, MonthlyUSD: ptr("300"), Reading: reading("150.00", now.Add(-7*24*time.Hour).Format(stampLayout))}
	if fail := (Batch{APICredits: []APICreditItem{unchanged}}).Check(stored, now); fail != nil {
		t.Errorf("an unchanged old reading: %v", fail)
	}
	// Changing its amount makes it a new reading, which the clock refuses.
	changed := unchanged
	changed.Reading = reading("149.00", unchanged.Reading.At)
	if fail := (Batch{APICredits: []APICreditItem{changed}}).Check(stored, now); fail == nil || fail.Code != CodeReadingTime {
		t.Errorf("a changed old reading: %v", fail)
	}
	// Only the first failure is reported, in item order.
	two := Batch{APICredits: []APICreditItem{{ID: "org-000000000001", MonthlyUSD: ptr("x")}, {ID: accountA, Renews: ptr("x")}}}
	if fail := two.Check(Values{}, now); fail == nil || fail.ID != "org-000000000001" {
		t.Errorf("first failure: %v", fail)
	}
}
