package credits

import (
	"encoding/json"
	mathrand "math/rand"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/settings"
)

// overlayCase is an input the overlay is checked over, and the earliest
// instant a state of it can be computed at: the meter's last save, since the
// publisher reads the meter before it computes.
type overlayCase struct {
	name  string
	in    Input
	floor time.Time
	// boundaries are the instants where something the overlay or valid_until
	// covers changes: a refill, a reading's time and its 31st day, stale-after
	// past the save, and five minutes past a stop. Each is tried with a
	// second either side.
	boundaries []time.Time
}

var overlayStaleAfters = []time.Duration{0, 15 * time.Minute, 45 * time.Minute, 2 * time.Hour}

// overlayCases is every fixture as published, the same with its meter stopped
// at its last save as quota-cache's final write leaves it, and readings with
// and without a cycle, one dated ahead.
func overlayCases(t *testing.T) []overlayCase {
	t.Helper()
	inputs := map[string]Input{}
	names := []string{}
	for _, f := range fixtures {
		in := inputOf(t, f)
		inputs[f.name], names = in, append(names, f.name)
		stopped := *in.Meter
		at := stopped.FlushedAt
		stopped.StoppedAt, stopped.StopReason = &at, client.MeterStopQuiesce
		in.Meter = &stopped
		inputs[f.name+" stopped"], names = in, append(names, f.name+" stopped")
	}
	inputs["readings"], names = readingsInput(t), append(names, "readings")

	cases := make([]overlayCase, 0, len(names))
	for _, name := range names {
		in := inputs[name]
		c := overlayCase{name: name, in: in, floor: in.Meter.FlushedAt}
		c.boundaries = append(c.boundaries, c.floor)
		for _, sa := range overlayStaleAfters {
			c.boundaries = append(c.boundaries, in.Meter.FlushedAt.Add(sa))
		}
		if in.Meter.StoppedAt != nil {
			c.boundaries = append(c.boundaries, in.Meter.StoppedAt.Add(client.MeterBriefGap))
		}
		for _, item := range in.Items {
			renews := []string{}
			if item.Credit != nil && item.Credit.Renews != "" {
				renews = append(renews, item.Credit.Renews)
			}
			if stored, ok := in.Settings.APICredits[item.ID]; ok {
				if stored.Renews != "" {
					renews = append(renews, stored.Renews)
				}
				if r := stored.Reading; r != nil {
					c.boundaries = append(c.boundaries, r.At, r.At.Add(readingWithoutCycle))
				}
			}
			for _, date := range renews {
				for months := range 3 {
					if cycle, err := client.CreditCycleAt(date, c.floor.AddDate(0, months, 0)); err == nil {
						c.boundaries = append(c.boundaries, cycle.End)
					}
				}
			}
		}
		around := []time.Time{}
		for _, b := range c.boundaries {
			around = append(around, b.Add(-time.Second), b, b.Add(time.Second))
		}
		slices.SortFunc(around, func(a, b time.Time) int { return a.Compare(b) })
		c.boundaries = slices.CompactFunc(around, func(a, b time.Time) bool { return a.Equal(b) })
		cases = append(cases, c)
	}
	return cases
}

// readingsInput is five organizations at the worked example's instant: a
// reading with no refill date, one dated ahead, the same without the meter
// counting the organization yet, and two that need settings.
func readingsInput(t *testing.T) Input {
	t.Helper()
	now := instant(t, workedNow)
	entries := []client.Entry{
		creditEntry("alpha", 0, fakeOrg("a"), "200", ""),
		creditEntry("bravo", 1, fakeOrg("b"), "200", "2026-10-29"),
		creditEntry("charlie", 2, fakeOrg("c"), "100", ""),
		creditEntry("delta", 3, fakeOrg("d"), "", "2026-10-15"),
		creditEntry("echo", 4, fakeOrg("e"), "150", ""),
	}
	values := settings.Values{Revision: 3, APICredits: map[string]settings.APICredit{
		entries[0].AuthIndex: *storedReading(t, fakeOrg("a"), "150.00", "2026-10-01T09:30:00Z"),
		entries[1].AuthIndex: *storedReading(t, fakeOrg("b"), "120.00", "2026-10-09T18:00:00Z"),
		entries[4].AuthIndex: *storedReading(t, fakeOrg("e"), "90.00", "2026-10-05T08:00:00Z"),
	}, Renewals: map[string]settings.Renewal{}}
	orgs := map[string]client.MeterOrganization{}
	for _, e := range entries[:4] {
		orgs[e.APICredit.OrganizationID] = healthyOrg(t)
	}
	return Input{Now: now, Items: itemsOf(entries...), Meter: meterOf(now.Add(-time.Minute), orgs), Settings: values,
		SettingsState: client.CreditSettingsOK, Version: "0.2.0"}
}

// checkOverlay reports whether the state published at t0 and judged at t1 is
// the state computed at t1 with staleAfter, as far as api_credits goes. It is
// vacuously true outside the overlay's domain: t0 before the floor, t1 before
// t0, or t1 at or past valid_until.
func checkOverlay(t *testing.T, c overlayCase, t0, t1 time.Time, staleAfter time.Duration) (checked, equal bool) {
	t.Helper()
	if t0.Before(c.floor) || t1.Before(t0) {
		return false, true
	}
	in := c.in
	in.Now, in.StaleAfter = t0, 0
	published := Compute(in)
	if published.ValidUntil != nil && !t1.Before(*published.ValidUntil) {
		return false, true
	}
	in.Now, in.StaleAfter = t1, staleAfter
	want := Compute(in).APICredits
	got := client.CreditsAt(published, t1, staleAfter).APICredits
	if reflect.DeepEqual(got, want) {
		return true, true
	}
	gotText, _ := json.Marshal(got)
	wantText, _ := json.Marshal(want)
	t.Errorf("%s published at %s, judged at %s with stale-after %v:\n overlay %s\nrecompute %s", c.name, t0, t1, staleAfter, gotText, wantText)
	return true, false
}

// The overlay is exactly recomputing: a state published at t0 and judged by
// client.CreditsAt at any later t1 before its valid_until, with any
// stale-after, is the state computed at t1 with that stale-after. This is
// what lets quota-cache publish once and readers judge freshness themselves.
func TestOverlayEqualsRecompute(t *testing.T) {
	cases := overlayCases(t)
	rng := rand.New(rand.NewPCG(20261011, 1))
	span := 40 * 24 * time.Hour
	instantFrom := func(c overlayCase, from time.Time, end time.Time) time.Time {
		switch rng.IntN(3) {
		case 0:
			return from.Add(time.Duration(rng.Int64N(int64(end.Sub(from)/time.Second)+1)) * time.Second)
		case 1:
			if i, _ := slices.BinarySearchFunc(c.boundaries, from, func(a, b time.Time) int { return a.Compare(b) }); i < len(c.boundaries) {
				return c.boundaries[i+rng.IntN(len(c.boundaries)-i)]
			}
		}
		return end.Add(-time.Second)
	}

	checked, failures := 0, 0
	// Every boundary as t1 against the floor, then random pairs, until 10,000
	// instants have been checked.
	for _, c := range cases {
		for _, b := range c.boundaries {
			for _, sa := range overlayStaleAfters {
				if ok, equal := checkOverlay(t, c, c.floor, b, sa); ok {
					checked++
					if !equal {
						failures++
					}
				}
			}
		}
	}
	for attempts := 0; checked < 10_000 && failures < 5 && attempts < 100_000; attempts++ {
		c := cases[rng.IntN(len(cases))]
		t0 := instantFrom(c, c.floor, c.floor.Add(span))
		in := c.in
		in.Now = t0
		end := t0.Add(span)
		if vu := Compute(in).ValidUntil; vu != nil && vu.Before(end) {
			end = *vu
		}
		if !end.After(t0) {
			continue
		}
		t1 := instantFrom(c, t0, end)
		if ok, equal := checkOverlay(t, c, t0, t1, overlayStaleAfters[rng.IntN(len(overlayStaleAfters))]); ok {
			checked++
			if !equal {
				failures++
			}
		}
	}
	if checked < 10_000 && failures == 0 {
		t.Fatalf("only %d instants could be checked", checked)
	}

	// And testing/quick over the same domain.
	property := func(which uint8, from, after uint32, stale uint8) bool {
		c := cases[int(which)%len(cases)]
		t0 := c.floor.Add(time.Duration(from%uint32(span/time.Second)) * time.Second)
		t1 := t0.Add(time.Duration(after%uint32(span/time.Second)) * time.Second)
		_, equal := checkOverlay(t, c, t0, t1, overlayStaleAfters[int(stale)%len(overlayStaleAfters)])
		return equal
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 1000, Rand: mathrand.New(mathrand.NewSource(20261011))}); err != nil {
		t.Error(err)
	}
}

// The overlay's domain is real: past valid_until, recomputing differs from
// the overlay, so a reader must treat the state as expired there.
func TestValidUntilIsWhereTheOverlayStops(t *testing.T) {
	for _, c := range overlayCases(t) {
		if c.name != "api-credits" && c.name != "readings" {
			continue
		}
		in := c.in
		in.Now = c.floor
		published := Compute(in)
		vu := *published.ValidUntil
		in.Now = vu
		if reflect.DeepEqual(client.CreditsAt(published, vu, 0).APICredits.Accounts, Compute(in).APICredits.Accounts) {
			t.Errorf("%s: nothing changes at valid_until %s", c.name, vu)
		}
	}
}
