package overrides

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// The two kinds of batch, one per card.
const (
	KindAPICredits = "apiCredits"
	KindRenewals   = "renewals"
)

// Batch bounds: one card's rows. Sixteen API credit rows is every
// organization quota-cache can link, and they fit the 4096 bytes a request
// may carry.
const (
	MaxAPICreditItems = 16
	MaxRenewalItems   = 32
)

// Batch is one press of Save: every changed row of one card, applied all or
// nothing.
type Batch struct {
	Kind       string
	APICredits []APICreditItem
	Renewals   []RenewalItem
	// Door is "console" or "password", for the log line a committed batch
	// writes.
	Door string
	// Credentials is every credential the served document lists. A renewal
	// date for one of them is never evicted to make room.
	Credentials map[string]bool
}

// APICreditItem is one API credit row's full desired state. A nil value is
// JSON null: not set here, Use config, or Clear.
type APICreditItem struct {
	ID           string
	BaseRevision string
	MonthlyUSD   *string
	Renews       *string
	Reading      *ReadingInput
	// OrganizationID is the organization the served document gives the
	// account, which a new reading stores. ConfigRenews is quota-cache's
	// configured refill date for it, "" when there is none, which decides
	// the cycle a new reading is checked against when Renews is nil. The
	// caller fills both from the document; neither comes from the request.
	OrganizationID string
	ConfigRenews   string
}

// ReadingInput is a Console reading as the page sends it.
type ReadingInput struct {
	RemainingUSD string
	At           string
}

// RenewalItem is one Claude credential's renewal date row.
type RenewalItem struct {
	ID           string
	BaseRevision string
	Date         *string
}

// IDs lists the batch's ids in request order.
func (b Batch) IDs() []string {
	ids := make([]string, 0, len(b.APICredits)+len(b.Renewals))
	for _, item := range b.APICredits {
		ids = append(ids, item.ID)
	}
	for _, item := range b.Renewals {
		ids = append(ids, item.ID)
	}
	return ids
}

// ErrInvalidRequest is every request that is not a well-formed batch.
var ErrInvalidRequest = errors.New("invalid_request")

// nullable is a JSON value that must be present, as a string or as null.
type nullable struct {
	set   bool
	value *string
}

func (n *nullable) UnmarshalJSON(raw []byte) error {
	n.set = true
	if string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	n.value = &s
	return nil
}

// required is a JSON string that must be present and not null.
type required struct {
	set   bool
	value string
}

func (r *required) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return errors.New("null")
	}
	r.set = true
	return json.Unmarshal(raw, &r.value)
}

type readingWire struct {
	RemainingUSD required `json:"remainingUsd"`
	At           required `json:"at"`
}

// readingField is the reading: present, and an object or null.
type readingField struct {
	set   bool
	value *readingWire
}

func (r *readingField) UnmarshalJSON(raw []byte) error {
	r.set = true
	if string(raw) == "null" {
		return nil
	}
	var reading readingWire
	if err := strictDecode(raw, &reading); err != nil {
		return err
	}
	if !reading.RemainingUSD.set || !reading.At.set {
		return errors.New("reading needs remainingUsd and at")
	}
	r.value = &reading
	return nil
}

// revisionShape is a baseRevision: "" for nothing stored, or a revision.
var revisionShape = regexp.MustCompile(`^(|[1-9][0-9]{0,19})$`)

// ParseBatch reads a batch from JSON: an object with exactly kind and items,
// every item carrying exactly its kind's keys, every value key present as a
// value or null, between one item and the kind's limit, and no id twice.
// Anything else is ErrInvalidRequest. The values themselves are judged by
// Check.
func ParseBatch(raw []byte) (Batch, error) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return Batch{}, ErrInvalidRequest
	}
	var envelope struct {
		Kind  required        `json:"kind"`
		Items json.RawMessage `json:"items"`
	}
	if strictDecode(raw, &envelope) != nil || !envelope.Kind.set || len(envelope.Items) == 0 {
		return Batch{}, ErrInvalidRequest
	}
	b := Batch{Kind: envelope.Kind.value}
	seen := map[string]bool{}
	fresh := func(id string, base required) bool {
		if id == "" || seen[id] || !base.set || !revisionShape.MatchString(base.value) {
			return false
		}
		seen[id] = true
		return true
	}
	switch b.Kind {
	case KindAPICredits:
		var items []struct {
			ID           required     `json:"id"`
			BaseRevision required     `json:"baseRevision"`
			MonthlyUSD   nullable     `json:"monthlyUsd"`
			Renews       nullable     `json:"renews"`
			Reading      readingField `json:"reading"`
		}
		if strictDecode(envelope.Items, &items) != nil || len(items) == 0 || len(items) > MaxAPICreditItems {
			return Batch{}, ErrInvalidRequest
		}
		for _, item := range items {
			if !item.ID.set || !fresh(item.ID.value, item.BaseRevision) ||
				!item.MonthlyUSD.set || !item.Renews.set || !item.Reading.set {
				return Batch{}, ErrInvalidRequest
			}
			out := APICreditItem{
				ID: item.ID.value, BaseRevision: item.BaseRevision.value,
				MonthlyUSD: item.MonthlyUSD.value, Renews: item.Renews.value,
			}
			if r := item.Reading.value; r != nil {
				out.Reading = &ReadingInput{RemainingUSD: r.RemainingUSD.value, At: r.At.value}
			}
			b.APICredits = append(b.APICredits, out)
		}
	case KindRenewals:
		var items []struct {
			ID           required `json:"id"`
			BaseRevision required `json:"baseRevision"`
			Date         nullable `json:"date"`
		}
		if strictDecode(envelope.Items, &items) != nil || len(items) == 0 || len(items) > MaxRenewalItems {
			return Batch{}, ErrInvalidRequest
		}
		for _, item := range items {
			if !item.ID.set || !fresh(item.ID.value, item.BaseRevision) || !item.Date.set {
				return Batch{}, ErrInvalidRequest
			}
			b.Renewals = append(b.Renewals, RenewalItem{ID: item.ID.value, BaseRevision: item.BaseRevision.value, Date: item.Date.value})
		}
	default:
		return Batch{}, ErrInvalidRequest
	}
	return b, nil
}

func strictDecode(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}

// FieldError is a value the save-time rules refuse: the code the page shows a
// message for, the row, and the field's JSON name.
type FieldError struct {
	Code  string
	ID    string
	Field string
}

func (e *FieldError) Error() string { return e.Code }

// Save-time rules and their codes.
const (
	CodeMonthlyUSD          = "invalid_monthly_usd"
	CodeRenews              = "invalid_renews"
	CodeDate                = "invalid_date"
	CodeReadingAmount       = "invalid_reading_amount"
	CodeReadingTime         = "invalid_reading_time"
	CodeReadingBeforeRefill = "reading_before_refill"
)

// The window a new reading's time must fall in, by the server's clock: Console
// readings are entered soon after they are taken, and the meter's hourly
// buckets only reach back so far (C.3). Five minutes ahead allows for a
// browser clock that runs a little fast.
const (
	readingMaxAge   = 48 * time.Hour
	readingMaxAhead = 5 * time.Minute
)

// Check applies the save-time rules to every item, in order, and returns the
// first failure, or nil. stored is what the store holds now: a reading resent
// unchanged, the same amount at the same instant, is not checked against the
// clock again, so a credit can be changed on a row whose reading is a week old.
func (b Batch) Check(stored Values, now time.Time) *FieldError {
	for _, item := range b.APICredits {
		fail := func(code, field string) *FieldError { return &FieldError{Code: code, ID: item.ID, Field: field} }
		if item.MonthlyUSD != nil && !qc.ValidMonthlyUSD(*item.MonthlyUSD) {
			return fail(CodeMonthlyUSD, "monthlyUsd")
		}
		if item.Renews != nil {
			if _, ok := ParseDate(*item.Renews); !ok {
				return fail(CodeRenews, "renews")
			}
		}
		r := item.Reading
		if r == nil {
			continue
		}
		if !qc.ValidMonthlyUSD(r.RemainingUSD) {
			return fail(CodeReadingAmount, "reading.remainingUsd")
		}
		if sameReading(stored.APICredits[item.ID].Reading, r) {
			continue
		}
		at, ok := parseStamp(r.At)
		if !ok || at.Before(now.Add(-readingMaxAge)) || at.After(now.Add(readingMaxAhead)) {
			return fail(CodeReadingTime, "reading.at")
		}
		renews := item.ConfigRenews
		if item.Renews != nil {
			renews = *item.Renews
		}
		if cycle, err := qc.CreditCycleAt(renews, now); err == nil && at.Before(cycle.Start) {
			return fail(CodeReadingBeforeRefill, "reading.at")
		}
	}
	for _, item := range b.Renewals {
		if item.Date != nil {
			if _, ok := ParseDate(*item.Date); !ok {
				return &FieldError{Code: CodeDate, ID: item.ID, Field: "date"}
			}
		}
	}
	return nil
}

// sameReading reports whether the page resent the stored reading unchanged.
func sameReading(stored *Reading, sent *ReadingInput) bool {
	if stored == nil || sent == nil {
		return stored == nil && sent == nil
	}
	at, ok := parseStamp(sent.At)
	return ok && sent.RemainingUSD == stored.RemainingUSD && at.Equal(stored.At)
}

// Clears reports whether the item sets nothing: every value null. An orphan,
// an entry for an account no longer listed, may only be cleared.
func (item APICreditItem) Clears() bool {
	return item.MonthlyUSD == nil && item.Renews == nil && item.Reading == nil
}

// Clears reports whether the item removes the renewal date.
func (item RenewalItem) Clears() bool { return item.Date == nil }
