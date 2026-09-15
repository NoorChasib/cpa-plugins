package aggregate

import (
	"testing"
	"time"
)

func credentialOf(t *testing.T, doc Document, id string) Credential {
	t.Helper()
	for _, credential := range doc.Credentials {
		if credential.ID == id {
			return credential
		}
	}
	t.Fatalf("document has no credential %s", id)
	return Credential{}
}

func activityIn(t *testing.T, doc Document, id string) *Activity {
	t.Helper()
	activity := credentialOf(t, doc, id).Activity
	if activity == nil {
		t.Fatalf("%s has no activity", id)
	}
	return activity
}

// The point of the whole feature: on a pool where one credential is carrying
// the traffic, the busy one must look busy and the quiet one must not. A strip
// scaled to its own row would ink a credential taking two requests exactly like
// the one taking twenty-one.
func TestIntensityIsScaledAcrossTheProviderNotTheRow(t *testing.T) {
	doc := buildFixture(t)
	hot := activityIn(t, doc, "claude-agency@example.com.json")
	quiet := activityIn(t, doc, "claude-siphorchannel@example.com.json")

	if got := hot.Buckets[18].Intensity; got != IntensityHigh {
		t.Fatalf("busiest bucket intensity = %d; want %d", got, IntensityHigh)
	}
	// One request against a provider peak of twenty-one is the bottom of the
	// ramp, not the top of its own row.
	if got := quiet.Buckets[16].Intensity; got != IntensityLow {
		t.Fatalf("single-request bucket intensity = %d; want %d", got, IntensityLow)
	}
	if quiet.Success != 1 || quiet.Failed != 2 {
		t.Fatalf("totals = %d ok / %d failed; want 1 / 2", quiet.Success, quiet.Failed)
	}
	if hot.Success != 117 {
		t.Fatalf("hot total = %d; want 117", hot.Success)
	}
}

func TestIntensityRamp(t *testing.T) {
	const peak = 100
	for _, tc := range []struct {
		total int64
		want  int
	}{
		{0, IntensityNone},
		{1, IntensityLow},
		{34, IntensityLow},
		{35, IntensityMedium},
		{67, IntensityMedium},
		{68, IntensityHigh},
		{100, IntensityHigh},
	} {
		if got := intensityOf(tc.total, peak); got != tc.want {
			t.Fatalf("intensityOf(%d, %d) = %d; want %d", tc.total, peak, got, tc.want)
		}
	}
	// A provider with no traffic at all has no scale, and dividing by it would
	// ink every empty bucket.
	if got := intensityOf(0, 0); got != IntensityNone {
		t.Fatalf("intensityOf(0, 0) = %d; want %d", got, IntensityNone)
	}
}

// live is the whole "right now" claim, and it comes from the bucket in progress
// rather than from anywhere in the window.
func TestLiveFollowsTheBucketInProgress(t *testing.T) {
	doc := buildFixture(t)
	if !activityIn(t, doc, "claude-agency@example.com.json").Live {
		t.Fatal("the credential taking requests in the current bucket is not live")
	}
	// Traffic two hours ago is not traffic now.
	if activityIn(t, doc, "claude-chasibnoor@example.com.json").Live {
		t.Fatal("a credential whose last request was two hours ago reports live")
	}
	if activityIn(t, doc, "claude-noor@example.com.json").Live {
		t.Fatal("an idle credential reports live")
	}
}

// A last-request time ahead of the clock would count up on a page whose every
// other number counts down.
func TestLastRequestIsNeverInTheFuture(t *testing.T) {
	doc := buildFixture(t)
	for _, credential := range doc.Credentials {
		if credential.Activity == nil || credential.Activity.LastRequestAtEpoch == nil {
			continue
		}
		if *credential.Activity.LastRequestAtEpoch > doc.GeneratedAtEpoch {
			t.Fatalf("%s reports its last request %ds after the document was built",
				credential.ID, *credential.Activity.LastRequestAtEpoch-doc.GeneratedAtEpoch)
		}
	}

	// An older bucket is dated at its own end rather than at now. The burst
	// stopped in bucket 7 of 0..19, which closed twelve buckets — 1h50m — back,
	// and that end is the tightest bound the ring supports: the requests
	// happened somewhere inside those ten minutes and it does not say where.
	burst := activityIn(t, doc, "claude-chasibnoor@example.com.json")
	if burst.LastRequestAtEpoch == nil {
		t.Fatal("a credential with traffic reports no last request")
	}
	if got := doc.GeneratedAtEpoch - *burst.LastRequestAtEpoch; got != 6600 {
		t.Fatalf("last request was %ds ago; want 6600 (1h50m)", got)
	}
}

// An empty ring and no ring at all are different facts: one says nothing has
// routed here in hours, the other says this host does not report routing.
func TestAnAbsentRingIsNullAndAnEmptyRingIsZeroes(t *testing.T) {
	doc := buildFixture(t)

	if got := credentialOf(t, doc, "xai-noor@example.com.json").Activity; got != nil {
		t.Fatal("a credential the host reported no ring for has an activity block")
	}

	idle := activityIn(t, doc, "claude-noor@example.com.json")
	if len(idle.Buckets) != 20 {
		t.Fatalf("idle ring has %d buckets; want 20", len(idle.Buckets))
	}
	if idle.Success != 0 || idle.Failed != 0 || idle.Live {
		t.Fatal("an idle ring reports traffic")
	}
	if idle.LastRequestAtEpoch != nil {
		t.Fatal("an idle ring reports a last request")
	}
	for i, bucket := range idle.Buckets {
		if bucket.Intensity != IntensityNone {
			t.Fatalf("idle bucket %d has intensity %d", i, bucket.Intensity)
		}
	}
}

// Width and span are read off the host's own labels. Hardcoding ten minutes
// would mislabel every strip the day CPA changed its ring, and the mislabelling
// would be invisible: the bars would look exactly the same.
func TestBucketWidthComesFromTheHostsLabels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ring  []RecentRequest
		width int
	}{
		{"ten minutes", tenMinuteRing(nil, nil), 600},
		{"five minutes", ringOf(12, 5*time.Minute, nil, nil), 300},
		{"one hour", ringOf(6, time.Hour, nil, nil), 3600},
		{"spanning midnight", []RecentRequest{{Label: "23:50-00:00"}}, 600},
		{"unparseable", []RecentRequest{{Label: "whenever"}}, defaultBucketSeconds},
		{"no label", []RecentRequest{{}}, defaultBucketSeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bucketSecondsOf(tc.ring); got != tc.width {
				t.Fatalf("bucketSecondsOf = %d; want %d", got, tc.width)
			}
		})
	}

	// End to end, on the fixture that deliberately does not use CPA's shape.
	doc := buildDegraded(t)
	odd := activityIn(t, doc, "codex-model@example.com.json")
	if odd.BucketSeconds != 300 || odd.WindowSeconds != 12*300 {
		t.Fatalf("ring reported as %ds x %ds; want 300 x %d", odd.BucketSeconds, odd.WindowSeconds, 12*300)
	}
}

// The ring arrives oldest first and is published that way. Reversing it would
// draw every strip backwards, and a strip is still a plausible-looking strip
// when it is backwards.
func TestRingKeepsTheHostsOrder(t *testing.T) {
	doc := buildFixture(t)
	hot := activityIn(t, doc, "claude-agency@example.com.json")
	if len(hot.Buckets) != 20 {
		t.Fatalf("ring has %d buckets; want 20", len(hot.Buckets))
	}
	for i, want := range []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 5, 9, 12, 8, 15, 17, 14, 21, 14} {
		if got := hot.Buckets[i].Success; got != want {
			t.Fatalf("bucket %d = %d; want %d — the ring is out of order", i, got, want)
		}
	}
	if hot.BucketSeconds != 600 || hot.WindowSeconds != 20*600 {
		t.Fatalf("ring reported as %ds x %d buckets", hot.BucketSeconds, hot.WindowSeconds)
	}
}

// A credential CPA has parked still shows the traffic that got it parked. The
// routing state is the credential's; the ring is a record of what happened.
func TestAParkedCredentialKeepsItsRing(t *testing.T) {
	doc := buildDegraded(t)
	parked := credentialOf(t, doc, "claude-unavailable@example.com.json")
	if parked.Status != StatusUnavailable {
		t.Fatalf("status = %q; want %q", parked.Status, StatusUnavailable)
	}
	if parked.Activity == nil || parked.Activity.Failed != 6 {
		t.Fatalf("a parked credential lost the failures in its ring: %+v", parked.Activity)
	}
}

// The ring carries no dates, only positions: its last bucket is whichever one
// is in progress when the host is asked. Every instant in the published block
// is therefore anchored to the clock the document is built with, and a ring
// read against a later clock describes a later window.
func TestRingIsAnchoredToTheClockItIsBuiltWith(t *testing.T) {
	ring := tenMinuteRing(map[int]int64{19: 4}, nil)
	now := time.Unix(fixtureNow, 0).UTC()

	current := activityOf(ring, 4, now)
	if !current.Live || current.LastRequestAtEpoch == nil || *current.LastRequestAtEpoch != now.Unix() {
		t.Fatalf("a request in the bucket in progress is dated %v, not now", current.LastRequestAtEpoch)
	}

	// One bucket width on, the same last bucket is one width on.
	later := activityOf(ring, 4, now.Add(10*time.Minute))
	if later.LastRequestAtEpoch == nil {
		t.Fatal("the last request went missing")
	}
	if moved := *later.LastRequestAtEpoch - *current.LastRequestAtEpoch; moved != 600 {
		t.Fatalf("the last request moved %ds for a 600s step", moved)
	}

	// Mid-bucket, the request in progress is still dated now rather than at the
	// end of a bucket that has not finished.
	partial := activityOf(ring, 4, now.Add(3*time.Minute))
	if *partial.LastRequestAtEpoch != now.Add(3*time.Minute).Unix() {
		t.Fatal("a request in the bucket in progress is dated at the bucket's end, which is in the future")
	}
}
