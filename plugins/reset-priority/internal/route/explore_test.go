package route

import (
	"slices"
	"testing"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// world drives one router through simulated time: the meter, the rebuilds
// that read it, and the picks.
type world struct {
	t testing.TB
	f fixture
	r *Router
}

func newWorld(t testing.TB) *world {
	w := &world{t: t, f: worked(t), r: New()}
	w.r.SetEnabled(true)
	w.rebuild(w.f.Now)
	return w
}

// rebuild is the router loop's rebuild at now: a freshly flushed meter and a
// freshly published credit state, a table built from them, published.
func (w *world) rebuild(now time.Time) {
	w.f.Meter.FlushedAt = now.Add(-30 * time.Second)
	w.r.Publish(Build(w.f.inputAt(now)), now)
}

// record is a usage record for the key reaching the meter at now. It teaches
// the meter nothing about the key's organization.
func (w *world) record(suffix string, now time.Time) {
	e := w.f.Meter.AuthIDs[id(suffix)]
	e.SeenAt = now
	w.f.Meter.AuthIDs[id(suffix)] = e
}

func (w *world) pick(now time.Time, sel string, suffixes ...string) Decision {
	return w.r.Pick(request(w.t, sel, suffixes...), now)
}

func TestPickExplores(t *testing.T) {
	t0 := at(t, "2026-10-11T12:00:00Z")

	t.Run("bootstrap: one exploration per pick, then unmapped", func(t *testing.T) {
		w := newWorld(t)
		w.f.Meter.AuthIDs = map[string]qc.MeterAuthID{}
		w.rebuild(t0)
		expect(t, w.r, request(t, "", "0b", "0d", "11"), t0, explored("0b"))
		expect(t, w.r, request(t, id("0b"), "0d", "11"), t0, explored("0d"))
		expect(t, w.r, request(t, id("0d"), "11"), t0, explored("11"))
		expect(t, w.r, request(t, "", "0b", "0d", "11"), t0, decline(ReasonUnmapped))
	})

	t.Run("a failover pick with a ranked candidate never explores", func(t *testing.T) {
		w := newWorld(t)
		expect(t, w.r, request(t, id("0d"), "0b", "12"), t0, routed("0b"))
		if _, ok := entryOf(w.r, "12"); ok {
			t.Fatal("a failover pick claimed an exploration slot")
		}
		expect(t, w.r, request(t, id("0d"), "0a", "12"), t0, explored("12"))
	})

	t.Run("waits", func(t *testing.T) {
		want := []time.Duration{10, 20, 40, 80, 160, 320, 360, 360}
		for step, minutes := range want {
			e := entry{claimedAt: t0.Unix(), step: uint8(min(step, maxStep))}
			end := t0.Add(minutes * time.Minute).Unix()
			if e.ready(end-1) || !e.ready(end) {
				t.Fatalf("step %d: ready at %v; want %v minutes", step, time.Unix(e.nextAt(), 0).Sub(t0), minutes)
			}
		}
	})

	t.Run("a claim that taught nothing doubles the wait", func(t *testing.T) {
		w := newWorld(t)
		claimed := t0
		expect(t, w.r, request(t, "", "0b", "12"), claimed, explored("12"))
		// Not ready before its wait.
		expect(t, w.r, request(t, "", "0b", "12"), claimed.Add(time.Minute), routed("0b"))
		for step := 1; step <= 7; step++ {
			// The attempt's usage record arrives, and the claim is judged
			// once it is QuickRearm old.
			w.record("12", claimed.Add(time.Second))
			w.rebuild(claimed.Add(QuickRearm - time.Second))
			if e, _ := entryOf(w.r, "12"); e.evaluated {
				t.Fatalf("step %d: judged before QuickRearm", step)
			}
			w.rebuild(claimed.Add(QuickRearm))
			e, _ := entryOf(w.r, "12")
			if want := uint8(min(step, maxStep)); e.step != want || !e.evaluated || e.quickPending {
				t.Fatalf("step %d: entry %+v", step, e)
			}
			next := claimed.Add(time.Duration(wait(e.step)) * time.Second)
			w.rebuild(next.Add(-time.Minute))
			expect(t, w.r, request(t, "", "0b", "12"), next.Add(-time.Second), routed("0b"))
			expect(t, w.r, request(t, "", "0b", "12"), next, explored("12"))
			claimed = next
		}
	})

	t.Run("a claim with no usage record gets one quick re-arm", func(t *testing.T) {
		w := newWorld(t)
		expect(t, w.r, request(t, "", "0b", "12"), t0, explored("12"))
		w.rebuild(t0.Add(QuickRearm))
		if e, _ := entryOf(w.r, "12"); e.step != 0 || !e.quickPending || !e.quickUsed {
			t.Fatalf("after a claim with no record: %+v", e)
		}
		expect(t, w.r, request(t, "", "0b", "12"), t0.Add(QuickRearm-time.Second), routed("0b"))
		// A pick naming a claim already judged does not mark it.
		expect(t, w.r, request(t, id("12"), "0b"), t0.Add(QuickRearm-time.Second), routed("0b"))
		if e, _ := entryOf(w.r, "12"); e.failedInBand {
			t.Fatal("a judged claim was marked failed in band")
		}
		expect(t, w.r, request(t, "", "0b", "12"), t0.Add(QuickRearm), explored("12"))
		// The re-arm claim uses up the re-arm: QuickRearm after it, before a
		// rebuild judges it, the key is not ready again.
		second := t0.Add(QuickRearm)
		expect(t, w.r, request(t, "", "0b", "12"), second.Add(QuickRearm), routed("0b"))
		// The second claim with no record advances the step.
		w.rebuild(second.Add(QuickRearm))
		if e, _ := entryOf(w.r, "12"); e.step != 1 || e.quickPending || e.quickUsed {
			t.Fatalf("after a second claim with no record: %+v", e)
		}
		expect(t, w.r, request(t, "", "0b", "12"), second.Add(2*QuickRearm), routed("0b"))
		w.rebuild(second.Add(20*time.Minute - time.Minute))
		expect(t, w.r, request(t, "", "0b", "12"), second.Add(20*time.Minute), explored("12"))
	})

	t.Run("an in-band failure advances the step with no quick re-arm", func(t *testing.T) {
		w := newWorld(t)
		expect(t, w.r, request(t, "", "0a", "12"), t0, explored("12"))
		expect(t, w.r, request(t, id("12"), "0a"), t0, decline(ReasonNoEligible))
		w.rebuild(t0.Add(QuickRearm))
		if e, _ := entryOf(w.r, "12"); e.step != 1 || e.quickPending || !e.evaluated {
			t.Fatalf("after an in-band failure: %+v", e)
		}
		// The judged claim is ready again after the second wait, not the
		// quick re-arm.
		expect(t, w.r, request(t, "", "0a", "12"), t0.Add(QuickRearm), decline(ReasonNoEligible))
		w.rebuild(t0.Add(19 * time.Minute))
		expect(t, w.r, request(t, "", "0a", "12"), t0.Add(20*time.Minute-time.Second), decline(ReasonNoEligible))
		expect(t, w.r, request(t, "", "0a", "12"), t0.Add(20*time.Minute), explored("12"))
	})

	t.Run("an entry goes when its key becomes ranked", func(t *testing.T) {
		w := newWorld(t)
		expect(t, w.r, request(t, "", "0b", "12"), t0, explored("12"))
		// The meter learns 12's organization: delta, and a success.
		w.f.Meter.Auths["0000000000000012"] = qc.MeterAuth{OrganizationID: fakeOrg("0d"), SeenAt: t0.Add(time.Second)}
		e := w.f.Meter.AuthIDs[id("12")]
		e.LastSuccessAt, e.Failures, e.SeenAt = ptr(t0.Add(time.Second)), 0, t0.Add(time.Second)
		w.f.Meter.AuthIDs[id("12")] = e
		w.rebuild(t0.Add(time.Minute))
		if _, ok := entryOf(w.r, "12"); ok {
			t.Fatal("the entry outlived the key becoming ranked")
		}
		expect(t, w.r, request(t, id("0b"), "0d", "12"), t0.Add(time.Minute), routed("0d"))
	})

	// An entry goes too when its key is mapped somewhere the router never
	// explores: to an organization the credit state does not list (unlinked),
	// or to none (foreign). The meter can learn either for an unmapped key,
	// and a failing key's organization can leave the credit state when the
	// operator removes it under Organizations on Quota Cache's page.
	for _, c := range []struct {
		name, suffix, class string
		want                keyClass
		move                func(w *world)
	}{
		{"an unmapped key is linked to an unlisted organization", "12", "unlinked", classUnlinked, func(w *world) {
			w.f.Meter.Auths["0000000000000012"] = qc.MeterAuth{OrganizationID: fakeOrg("99"), SeenAt: t0.Add(time.Second)}
		}},
		{"an unmapped key turns out foreign", "12", "foreign", classForeign, func(w *world) {
			w.f.Meter.Auths["0000000000000012"] = qc.MeterAuth{OrganizationID: "", SeenAt: t0.Add(time.Second)}
		}},
		{"a failing key's organization is no longer listed", "0b", "unlinked", classUnlinked, func(w *world) {
			w.f.Accounts = slices.Delete(w.f.Accounts, 1, 2) // bravo
		}},
	} {
		t.Run("an entry goes when "+c.name, func(t *testing.T) {
			w := newWorld(t)
			if c.suffix == "0b" {
				failing(w, "0b")
			}
			expect(t, w.r, request(t, "", "0d", c.suffix), t0, explored(c.suffix))
			c.move(w)
			w.rebuild(t0.Add(time.Minute))
			table := w.r.table.Load()
			if k := table.keys[id(c.suffix)]; table.Reason != ReasonNone || k.class != c.want || k.org != -1 {
				t.Fatalf("table reason %s, key %+v; want an active table and %s %s", table.Reason, k, c.suffix, c.class)
			}
			if _, ok := entryOf(w.r, c.suffix); ok {
				t.Fatalf("%s's entry outlived it becoming %s", c.suffix, c.class)
			}
			if exploring := w.r.Status(t0.Add(time.Minute)).Exploring; len(exploring) != 0 {
				t.Fatalf("the status still lists %+v", exploring)
			}
		})
	}

	// A re-checked key whose organization leaves the credit state has no
	// organization to read. Were it read, the rebuild would panic, the router
	// loop would record build_failed, and every rebuild after would do the
	// same until the entry was MaxExploreAge old: no routing for a week.
	t.Run("a re-check entry goes when its organization is no longer listed", func(t *testing.T) {
		w := newWorld(t)
		w.f.Accounts[0].EstimateLeftText = "$6.20" // alpha is re-checked
		w.rebuild(t0)
		expect(t, w.r, request(t, "", "0a", "0b"), t0, rechecked("0a"))
		// The operator removes alpha under Organizations on Quota Cache's page.
		w.f.Accounts = w.f.Accounts[1:]
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("the rebuild panicked: %v", p)
				}
			}()
			w.rebuild(t0.Add(time.Minute))
		}()
		table := w.r.table.Load()
		if k := table.keys[id("0a")]; table.Reason != ReasonNone || k.class != classUnlinked || k.org != -1 {
			t.Fatalf("table reason %s, key %+v; want an active table and 0a unlinked", table.Reason, k)
		}
		if _, ok := entryOf(w.r, "0a"); ok {
			t.Fatal("0a's re-check entry outlived alpha leaving the credit state")
		}
		expect(t, w.r, request(t, "", "0a", "0b"), t0.Add(time.Minute), routed("0b"))
	})

	// bravo's state between rebuilds. An organization can be ineligible for a
	// while as the table stays active, out until a re-check succeeds for one;
	// stale here stands for any such spell.
	bravo := func(w *world, state string) { w.f.Accounts[1].State = state }

	t.Run("an organization ineligible for a while keeps a failing key's step", func(t *testing.T) {
		w := newWorld(t)
		failing(w, "0b")
		// Two claims that taught nothing put 0b at step 2.
		claimed := t0
		for step := uint8(1); step <= 2; step++ {
			w.rebuild(claimed)
			expect(t, w.r, request(t, "", "0b", "0d"), claimed, explored("0b"))
			w.record("0b", claimed.Add(time.Second))
			w.rebuild(claimed.Add(QuickRearm))
			if e, _ := entryOf(w.r, "0b"); e.step != step || !e.evaluated {
				t.Fatalf("step %d: entry %+v", step, e)
			}
			if step < 2 {
				claimed = claimed.Add(time.Duration(wait(step)) * time.Second)
			}
		}
		// One rebuild sees bravo stale, then out, then it is fresh again.
		for _, state := range []string{qc.CreditStateStale, qc.CreditStateOut} {
			bravo(w, state)
			w.rebuild(claimed.Add(10 * time.Minute))
			if e, ok := entryOf(w.r, "0b"); !ok || e.step != 2 {
				t.Fatalf("bravo %s: entry %+v, kept %v; want it at step 2", state, e, ok)
			}
		}
		bravo(w, qc.CreditStateOK)
		next := claimed.Add(time.Duration(wait(2)) * time.Second)
		w.rebuild(next.Add(-time.Minute))
		expect(t, w.r, request(t, "", "0b", "0d"), next.Add(-time.Second), routed("0d"))
		expect(t, w.r, request(t, "", "0b", "0d"), next, explored("0b"))
	})

	t.Run("a claim judged while its organization is ineligible still backs off", func(t *testing.T) {
		w := newWorld(t)
		failing(w, "0b")
		expect(t, w.r, request(t, "", "0b", "0d"), t0, explored("0b"))
		w.record("0b", t0.Add(time.Second))
		bravo(w, qc.CreditStateStale)
		w.rebuild(t0.Add(QuickRearm))
		if e, ok := entryOf(w.r, "0b"); !ok || e.step != 1 || !e.evaluated {
			t.Fatalf("judged while bravo was stale: entry %+v, kept %v", e, ok)
		}
		bravo(w, qc.CreditStateOK)
		w.rebuild(t0.Add(19 * time.Minute))
		expect(t, w.r, request(t, "", "0b", "0d"), t0.Add(20*time.Minute-time.Second), routed("0d"))
		expect(t, w.r, request(t, "", "0b", "0d"), t0.Add(20*time.Minute), explored("0b"))
	})

	t.Run("a re-check keeps its step while its organization is stale", func(t *testing.T) {
		w := newWorld(t)
		// alpha is out with $6.20 left in its estimate: re-checked.
		w.f.Accounts[0].EstimateLeftText = "$6.20"
		w.rebuild(t0)
		expect(t, w.r, request(t, "", "0a", "0b"), t0, rechecked("0a"))
		w.record("0a", t0.Add(time.Second)) // refused again
		w.rebuild(t0.Add(QuickRearm))
		w.f.Accounts[0].State = qc.CreditStateStale
		w.rebuild(t0.Add(10 * time.Minute))
		if e, ok := entryOf(w.r, "0a"); !ok || e.step != 1 || e.kind != kindRecheck {
			t.Fatalf("alpha stale: entry %+v, kept %v; want the re-check at step 1", e, ok)
		}
		w.f.Accounts[0].State = qc.CreditStateOut
		w.rebuild(t0.Add(19 * time.Minute))
		expect(t, w.r, request(t, "", "0a", "0b"), t0.Add(20*time.Minute-time.Second), routed("0b"))
		expect(t, w.r, request(t, "", "0a", "0b"), t0.Add(20*time.Minute), rechecked("0a"))
	})

	// An inactive table holds no keys. Were it judged, every re-check would
	// go as moot, which resets its wait, and every claim with no usage record
	// would spend its quick re-arm on a spell that says nothing about it.
	t.Run("an inactive table judges no claim", func(t *testing.T) {
		w := newWorld(t)
		w.f.Accounts[0].EstimateLeftText = "$6.20" // alpha is re-checked
		w.rebuild(t0)
		expect(t, w.r, request(t, "", "0a", "0b", "12"), t0, explored("12"))
		expect(t, w.r, request(t, "", "0a", "0b", "12"), t0, rechecked("0a"))
		w.record("0a", t0.Add(time.Second)) // refused again
		// Quota Cache restarts: its credit state is older than
		// MaxCreditStateAge at the rebuild, by when both claims are QuickRearm
		// old.
		in := w.f.inputAt(t0.Add(QuickRearm))
		in.Credits.ComputedAt = in.Now.Add(-MaxCreditStateAge - time.Minute)
		table := Build(in)
		if table.Reason != ReasonCreditsStale {
			t.Fatalf("table reason %s; want credits_stale", table.Reason)
		}
		w.r.Publish(table, in.Now)
		for _, suffix := range []string{"12", "0a"} {
			if e, ok := entryOf(w.r, suffix); !ok || e.step != 0 || e.evaluated || e.quickPending || e.quickUsed {
				t.Fatalf("%s after an inactive table: entry %+v, kept %v; want it untouched", suffix, e, ok)
			}
		}
		// The next active table judges both as usual: 12 left no usage
		// record, so it gets its quick re-arm; 0a's attempt reached the
		// meter, so it backs off.
		w.rebuild(t0.Add(QuickRearm + time.Minute))
		if e, ok := entryOf(w.r, "12"); !ok || e.step != 0 || !e.quickPending || !e.evaluated {
			t.Fatalf("12 after the next active table: entry %+v, kept %v; want a quick re-arm", e, ok)
		}
		if e, ok := entryOf(w.r, "0a"); !ok || e.step != 1 || e.kind != kindRecheck || !e.evaluated {
			t.Fatalf("0a after the next active table: entry %+v, kept %v; want the re-check at step 1", e, ok)
		}
	})

	// The bound holds when the key's organization keeps flipping: here bravo
	// reads stale for minutes 3 to 7 of every 10, so some claims are even
	// judged while it is stale.
	t.Run("a day of a failing key whose organization keeps going stale", func(t *testing.T) {
		w := newWorld(t)
		failing(w, "0b")
		claims := [2]int{}
		for tick := range 2 * 24 * 120 { // every 30 seconds for two days
			now := t0.Add(time.Duration(tick) * 30 * time.Second)
			if tick%2 == 0 {
				state := qc.CreditStateOK
				if minute := tick / 2 % 10; minute >= 3 && minute < 8 {
					state = qc.CreditStateStale
				}
				bravo(w, state)
				w.rebuild(now)
			}
			d := w.pick(now, "", "0b", "0d")
			if d == explored("0b") {
				claims[tick/(24*120)]++
				w.record("0b", now)
			} else if d != routed("0d") {
				t.Fatalf("tick %d: {%q %s}", tick, d.AuthID, d.Reason)
			}
		}
		t.Logf("claims: %d on the first day, %d on the second", claims[0], claims[1])
		if claims[0] != 8 || claims[1] != 4 {
			t.Fatalf("claimed %d then %d times; the waits give 8 then 4, as when bravo never flips", claims[0], claims[1])
		}
	})

	t.Run("the 65th ID evicts the oldest claim", func(t *testing.T) {
		r := New()
		for n := range MaxExplore {
			// Two IDs share the oldest claim; the smaller goes.
			claimedAt := t0.Add(time.Duration(n) * time.Second)
			if n == 1 {
				claimedAt = t0
			}
			if !r.claim(id(hex2(n)), kindUnmapped, "", claimedAt) {
				t.Fatalf("claim %d failed", n)
			}
		}
		if !r.claim(id("ff"), kindUnmapped, "", t0.Add(time.Hour)) {
			t.Fatal("the 65th claim failed")
		}
		if len(r.explore.m) != MaxExplore {
			t.Fatalf("%d entries; want %d", len(r.explore.m), MaxExplore)
		}
		if _, ok := entryOf(r, hex2(0)); ok {
			t.Fatal("the oldest claim with the smaller ID was kept")
		}
		if _, ok := entryOf(r, hex2(1)); !ok {
			t.Fatal("the larger ID of the oldest claims was evicted")
		}
	})

	t.Run("entries are dropped after MaxExploreAge, never by Publish(nil)", func(t *testing.T) {
		w := newWorld(t)
		expect(t, w.r, request(t, "", "0b", "12"), t0, explored("12"))
		w.r.Publish(nil, t0.Add(30*24*time.Hour))
		if _, ok := entryOf(w.r, "12"); !ok {
			t.Fatal("Publish(nil) dropped an entry")
		}
		w.rebuild(t0.Add(MaxExploreAge))
		if _, ok := entryOf(w.r, "12"); !ok {
			t.Fatal("an entry exactly MaxExploreAge old was dropped")
		}
		w.rebuild(t0.Add(MaxExploreAge + time.Second))
		if _, ok := entryOf(w.r, "12"); ok {
			t.Fatal("an entry older than MaxExploreAge was kept")
		}
	})

	// The bound on a key that never becomes proven: at most 9 claims in its
	// first day and 4 a day after when its attempts reach the meter, and 16
	// and 8 when none does. The waits give exactly what REFERENCE.md states:
	// 8 and 4, or 16 and 8.
	for _, c := range []struct {
		name                 string
		records, inBand      bool
		firstDay, everyDay   int
		exactFirst, exactDay int
	}{
		{"with records", true, false, 9, 4, 8, 4},
		{"with records and in-band failures", true, true, 9, 4, 8, 4},
		{"without records", false, false, 16, 8, 16, 8},
	} {
		t.Run("a day of a key that never becomes proven, "+c.name, func(t *testing.T) {
			w := newWorld(t)
			claims := [2]int{}
			for tick := range 2 * 24 * 120 { // every 30 seconds for two days
				now := t0.Add(time.Duration(tick) * 30 * time.Second)
				if tick%2 == 0 {
					w.rebuild(now)
				}
				d := w.pick(now, "", "0b", "12")
				if d == explored("12") {
					claims[tick/(24*120)]++
					if c.records {
						w.record("12", now)
					}
					if c.inBand {
						w.pick(now, id("12"), "0b")
					}
				} else if d != routed("0b") {
					t.Fatalf("tick %d: {%q %s}", tick, d.AuthID, d.Reason)
				}
			}
			t.Logf("claims: %d on the first day, %d on the second", claims[0], claims[1])
			if claims[0] > c.firstDay || claims[1] > c.everyDay {
				t.Fatalf("claimed %d then %d times; the bound allows %d then %d", claims[0], claims[1], c.firstDay, c.everyDay)
			}
			if claims[0] != c.exactFirst || claims[1] != c.exactDay {
				t.Fatalf("claimed %d then %d times; the waits give %d then %d", claims[0], claims[1], c.exactFirst, c.exactDay)
			}
		})
	}
}

// failing makes a worked-example key unproven, two failures since its last
// success, and rebuilds at the world's start.
func failing(w *world, suffix string) {
	e := w.f.Meter.AuthIDs[id(suffix)]
	e.Failures = 2
	w.f.Meter.AuthIDs[id(suffix)] = e
	w.rebuild(w.f.Now)
}

// hex2 is n as two hex digits, for an auth ID suffix.
func hex2(n int) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[n/16%16], digits[n%16]})
}
