package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The limits and the stop reasons are written into the meter file and read by
// quota-glance, so their values are the contract.
func TestMeterConstants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"MeterSchema", MeterSchema, 1},
		{"MeterDays", MeterDays, 40},
		{"MeterHours", MeterHours, 72},
		{"MaxMeterOrganizations", MaxMeterOrganizations, 32},
		{"MaxMeterUsagePerBucket", MaxMeterUsagePerBucket, 16},
		{"MaxMeterUsageEntries", MaxMeterUsageEntries, 6000},
		{"MaxMeterUnlinked", MaxMeterUnlinked, 8},
		{"MaxMeterAuths", MaxMeterAuths, 64},
		{"MaxMeterGaps", MaxMeterGaps, 8},
		{"MaxMeterBytes", MaxMeterBytes, 2097152},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	for got, want := range map[string]string{
		MeterOtherModel:     "(other)",
		MeterPromptOver100K: "over_100k",
		MeterStopShutdown:   "shutdown",
		MeterStopQuiesce:    "quiesce",
		MeterStopDisabled:   "disabled",
		MeterStopNoItems:    "no_items",
		MeterStopFailed:     "failed",
		MeterGapUncleanStop: "unclean_stop",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
	if MeterBriefGap != 5*time.Minute {
		t.Errorf("MeterBriefGap = %s", MeterBriefGap)
	}
}

func TestMeterPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"snapshot.json", "snapshot.meter.json"},
		{"x", "x.meter.json"},
		{"a.b/c.json", "a.b/c.meter.json"},
		{"a.json/c", "a.json/c.meter.json"},
		{DefaultPath, "plugins/data/quota-cache/snapshot.meter.json"},
	} {
		if got := MeterPath(tc.in); got != tc.want {
			t.Errorf("MeterPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Only a regular file of at most MaxMeterBytes, holding a meter of the one
// schema, is read. No file at all is told apart from a file that cannot be
// used.
func TestLoadMeterRefuses(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := LoadMeter(filepath.Join(dir, "none.meter.json")); !errors.Is(err, ErrMeterMissing) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := LoadMeter(filepath.Join(dir, "no-dir", "snapshot.meter.json")); !errors.Is(err, ErrMeterMissing) {
		t.Fatalf("missing directory: %v", err)
	}

	minimal := `{"schema":1}`
	good := write("good.meter.json", minimal)
	if _, err := LoadMeter(good); err != nil {
		t.Fatal(err)
	}
	// Whitespace keeps the JSON valid, so only the size decides.
	exact := write("exact.meter.json", minimal+strings.Repeat(" ", MaxMeterBytes-len(minimal)))
	if _, err := LoadMeter(exact); err != nil {
		t.Fatalf("a file of exactly MaxMeterBytes: %v", err)
	}
	link := filepath.Join(dir, "link.meter.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(dir, "directory.meter.json")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"oversize":  write("oversize.meter.json", minimal+strings.Repeat(" ", MaxMeterBytes+1-len(minimal))),
		"bad JSON":  write("bad.meter.json", `{"schema":1,`),
		"schema 2":  write("schema2.meter.json", `{"schema":2}`),
		"no schema": write("noschema.meter.json", `{}`),
		"null":      write("null.meter.json", `null`),
		"an array":  write("array.meter.json", `[]`),
		"a symlink": link,
		"directory": directory,
	} {
		if meter, err := LoadMeter(path); !errors.Is(err, ErrUnavailable) || meter != nil {
			t.Errorf("%s: meter %v, err %v; want ErrUnavailable", name, meter, err)
		}
	}
}

// A reader can range over and index into a loaded meter without checking for
// nil, whatever the file left out or wrote as null.
func TestLoadMeterFillsNilMapsAndSlices(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"absent": `{"schema":1,"organizations":{"00000000-0000-4000-8000-00000000000a":{"since":"2026-10-09T00:00:00Z","hours":[{"start":"2026-10-09T11:00:00Z"}]}}}`,
		"null": `{"schema":1,"gaps":null,"unlinked":null,"auths":null,"organizations":{"00000000-0000-4000-8000-00000000000a":` +
			`{"since":"2026-10-09T00:00:00Z","days":null,"hours":[{"start":"2026-10-09T11:00:00Z","usage":null}]}}}`,
	} {
		path := filepath.Join(dir, name+".meter.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		meter, err := LoadMeter(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if meter.Gaps == nil || meter.Unlinked == nil || meter.Auths == nil || meter.Organizations == nil {
			t.Fatalf("%s: nil collection in %+v", name, meter)
		}
		org := meter.Organizations["00000000-0000-4000-8000-00000000000a"]
		if org.Days == nil || len(org.Days) != 0 || len(org.Hours) != 1 || org.Hours[0].Usage == nil {
			t.Fatalf("%s: organization = %+v", name, org)
		}
	}
	path := filepath.Join(dir, "empty.meter.json")
	if err := os.WriteFile(path, []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	meter, err := LoadMeter(path)
	if err != nil {
		t.Fatal(err)
	}
	if meter.Organizations == nil || len(meter.Organizations) != 0 || meter.Gaps == nil || meter.Unlinked == nil || meter.Auths == nil {
		t.Fatalf("empty meter = %+v", meter)
	}
}

// Buckets are UTC days and hours whatever zone the instant arrives in,
// including a zone half an hour off UTC.
func TestMeterBucketStartsIgnoreTheCallersZone(t *testing.T) {
	newYork := time.FixedZone("EDT", -4*3600)
	kolkata := time.FixedZone("IST", 5*3600+1800)
	for _, tc := range []struct {
		name      string
		at        time.Time
		day, hour time.Time
	}{
		{"UTC", time.Date(2026, 10, 9, 13, 20, 45, 5, time.UTC), utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 13, 0)},
		{"midnight", utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 0, 0)},
		{"last instant of the day", utc(2026, 10, 10, 0, 0).Add(-time.Nanosecond), utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 23, 0)},
		{"New York evening is the next UTC day", time.Date(2026, 10, 8, 22, 30, 0, 0, newYork), utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 2, 0)},
		{"a half-hour zone", time.Date(2026, 10, 9, 8, 15, 0, 0, kolkata), utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 2, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			day, hour := MeterDayStart(tc.at), MeterHourStart(tc.at)
			if !day.Equal(tc.day) || !hour.Equal(tc.hour) {
				t.Fatalf("day %s hour %s, want %s and %s", day, hour, tc.day, tc.hour)
			}
			if day.Location() != time.UTC || hour.Location() != time.UTC {
				t.Fatal("bucket starts must be in UTC")
			}
		})
	}
}

func TestMeterPromptClass(t *testing.T) {
	for _, tc := range []struct {
		model  string
		tokens uint64
		want   string
	}{
		{"claude-haiku-5-5", 100000, ""},
		{"claude-haiku-5-5", 100001, MeterPromptOver100K},
		{"claude-haiku-5-5-20261001", 100001, MeterPromptOver100K},
		{"claude-haiku-5-5", math.MaxUint64, MeterPromptOver100K},
		{"claude-haiku-5-5", 0, ""},
		{"claude-sonnet-5-5", 900000, ""},
		{"claude-haiku-4-5", 900000, ""},
		{MeterOtherModel, 900000, ""},
	} {
		if got := MeterPromptClass(tc.model, tc.tokens); got != tc.want {
			t.Errorf("MeterPromptClass(%q, %d) = %q, want %q", tc.model, tc.tokens, got, tc.want)
		}
	}
}

func TestNormalizeMeterModel(t *testing.T) {
	long := "claude-" + strings.Repeat("x", 57)
	for _, tc := range []struct{ model, response, want string }{
		{"claude-sonnet-5-5", "claude-sonnet-5-5-20260901", "claude-sonnet-5-5-20260901"},
		{"claude-opus-5-5", "", "claude-opus-5-5"},
		{"claude-opus-5-5", " \t", "claude-opus-5-5"},
		{"  Claude-Opus-5-5 ", "", "claude-opus-5-5"},
		{"x", " CLAUDE-HAIKU-5-5 ", "claude-haiku-5-5"},
		{"claude-haiku-4-5@20251001", "", "claude-haiku-4-5@20251001"},
		{"anthropic.claude-3-5-haiku-20241022-v1:0", "", "anthropic.claude-3-5-haiku-20241022-v1:0"},
		{"vendor/claude_fable", "", "vendor/claude_fable"},
		{long, "", long},
		// Junk is kept under one name, never as itself.
		{"", "", MeterOtherModel},
		{long + "x", "", MeterOtherModel},
		{"claude opus", "", MeterOtherModel},
		{"-claude", "", MeterOtherModel},
		{".claude", "", MeterOtherModel},
		{"claude-é", "", MeterOtherModel},
		{"claude\n5", "", MeterOtherModel},
		{`claude"5`, "", MeterOtherModel},
		{MeterOtherModel, "", MeterOtherModel},
		// A response model, once named, is the one judged.
		{"claude-opus-5-5", "bad model", MeterOtherModel},
	} {
		got := NormalizeMeterModel(tc.model, tc.response)
		if got != tc.want {
			t.Errorf("NormalizeMeterModel(%q, %q) = %q, want %q", tc.model, tc.response, got, tc.want)
		}
		// What it returns is what it keeps: a stored name passes unchanged.
		if again := NormalizeMeterModel(got, ""); again != got {
			t.Errorf("NormalizeMeterModel(%q) = %q, not stable", got, again)
		}
	}
}

func TestMergeMeterUsage(t *testing.T) {
	var usage []MeterUsage
	for _, u := range []MeterUsage{
		{Model: "claude-sonnet-5-5", Requests: 1, Input: 10},
		{Model: "claude-haiku-5-5", Prompt: MeterPromptOver100K, Failed: 1, Input: 200000},
		{Model: "claude-haiku-5-5", Requests: 2, Output: 5},
		{Model: "claude-sonnet-5-5", Requests: 1, Failed: 1, Input: 5, Output: 1, CacheRead: 2, CacheWrite: 3},
	} {
		usage = MergeMeterUsage(usage, u)
	}
	want := []MeterUsage{
		{Model: "claude-haiku-5-5", Requests: 2, Output: 5},
		{Model: "claude-haiku-5-5", Prompt: MeterPromptOver100K, Failed: 1, Input: 200000},
		{Model: "claude-sonnet-5-5", Requests: 2, Failed: 1, Input: 15, Output: 1, CacheRead: 2, CacheWrite: 3},
	}
	if !slices.Equal(usage, want) {
		t.Fatalf("usage = %+v\nwant %+v", usage, want)
	}

	most := uint64(math.MaxUint64)
	full := MeterUsage{Model: "claude-opus-5-5", Requests: most - 1, Failed: most, Input: most - 2, Output: most, CacheRead: most, CacheWrite: 1}
	usage = MergeMeterUsage([]MeterUsage{full}, MeterUsage{Model: "claude-opus-5-5", Requests: 5, Failed: 1, Input: 3, Output: most, CacheRead: 1, CacheWrite: most})
	if got := usage[0]; len(usage) != 1 || got.Requests != most || got.Failed != most || got.Input != most ||
		got.Output != most || got.CacheRead != most || got.CacheWrite != most {
		t.Fatalf("saturated usage = %+v", usage)
	}
}

// A cycle's usage is every day bucket from its start, summed per model and
// prompt class; the organization's own buckets are left as they were.
func TestUsageFromDay(t *testing.T) {
	org := MeterOrganization{Days: []MeterBucket{
		{Start: utc(2026, 9, 30, 0, 0), Usage: []MeterUsage{{Model: "claude-sonnet-5-5", Requests: 1, Input: 100}}},
		{Start: utc(2026, 10, 1, 0, 0), Usage: []MeterUsage{
			{Model: "claude-haiku-5-5", Requests: 1, Input: 5},
			{Model: "claude-haiku-5-5", Prompt: MeterPromptOver100K, Requests: 1, Input: 150000},
			{Model: "claude-sonnet-5-5", Requests: 1, Input: 10, Output: 1},
		}},
		{Start: utc(2026, 10, 3, 0, 0), Usage: []MeterUsage{
			{Model: "claude-opus-5-5", Requests: 1, Input: 1},
			{Model: "claude-sonnet-5-5", Failed: 1, Input: 20, CacheRead: 7, CacheWrite: 9},
		}},
	}}
	got := org.UsageFromDay(utc(2026, 10, 1, 0, 0))
	want := []MeterUsage{
		{Model: "claude-haiku-5-5", Requests: 1, Input: 5},
		{Model: "claude-haiku-5-5", Prompt: MeterPromptOver100K, Requests: 1, Input: 150000},
		{Model: "claude-opus-5-5", Requests: 1, Input: 1},
		{Model: "claude-sonnet-5-5", Requests: 1, Failed: 1, Input: 30, Output: 1, CacheRead: 7, CacheWrite: 9},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("usage = %+v\nwant %+v", got, want)
	}
	if org.Days[1].Usage[2].Input != 10 || org.Days[0].Usage[0].Input != 100 {
		t.Fatal("summing changed the organization's buckets")
	}
	if all := org.UsageFromDay(time.Time{}); all[len(all)-1].Input != 130 {
		t.Fatalf("every day = %+v", all)
	}
	for _, empty := range [][]MeterUsage{org.UsageFromDay(utc(2026, 10, 4, 0, 0)), (MeterOrganization{}).UsageFromDay(utc(2026, 10, 1, 0, 0))} {
		if empty == nil || len(empty) != 0 {
			t.Fatalf("no days = %#v, want an empty slice", empty)
		}
	}
}

// A reading's baseline is the hour buckets of [from, to). Coverage says whether
// the meter flushed at flushedAt keeps every hour of that window: from no
// earlier than its oldest kept hour, to no later than the end of its hour.
func TestUsageInHours(t *testing.T) {
	hour := func(at time.Time, input uint64) MeterBucket {
		return MeterBucket{Start: at, Usage: []MeterUsage{{Model: "claude-sonnet-5-5", Requests: 1, Input: input}}}
	}
	org := MeterOrganization{Hours: []MeterBucket{
		hour(utc(2026, 10, 8, 23, 0), 16),
		hour(utc(2026, 10, 9, 0, 0), 1),
		hour(utc(2026, 10, 9, 9, 0), 2),
		hour(utc(2026, 10, 9, 12, 0), 4),
		hour(utc(2026, 10, 9, 13, 0), 8),
	}}
	flushed := utc(2026, 10, 9, 13, 19)
	usage, covered := org.UsageInHours(utc(2026, 10, 9, 0, 0), utc(2026, 10, 9, 13, 0), flushed)
	if want := []MeterUsage{{Model: "claude-sonnet-5-5", Requests: 3, Input: 7}}; !covered || !slices.Equal(usage, want) {
		t.Fatalf("usage = %+v covered %v, want %+v covered", usage, covered, want)
	}

	oldest, end := utc(2026, 10, 6, 13, 0), utc(2026, 10, 9, 14, 0)
	for _, tc := range []struct {
		name     string
		from, to time.Time
		covered  bool
	}{
		{"the whole kept span", oldest, end, true},
		{"one second before the oldest kept hour", oldest.Add(-time.Second), end, false},
		{"an hour before the oldest kept hour", oldest.Add(-time.Hour), end, false},
		{"one second past the flush hour", oldest, end.Add(time.Second), false},
		{"an hour past the flush hour", utc(2026, 10, 9, 0, 0), end.Add(time.Hour), false},
		{"an empty window", end, end, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage, covered := org.UsageInHours(tc.from, tc.to, flushed)
			if covered != tc.covered || usage == nil {
				t.Fatalf("covered = %v (usage %#v), want %v", covered, usage, tc.covered)
			}
		})
	}
	if usage, _ := org.UsageInHours(oldest, end, flushed); len(usage) != 1 || usage[0].Input != 31 || usage[0].Requests != 5 {
		t.Fatalf("whole span = %+v", usage)
	}
	// The flush time's zone does not move the kept span.
	newYork := time.FixedZone("EDT", -4*3600)
	if _, covered := org.UsageInHours(oldest, end, flushed.In(newYork)); !covered {
		t.Fatal("a flush time in another zone moved the kept span")
	}
}

func TestMeterGapBrief(t *testing.T) {
	from := utc(2026, 10, 9, 9, 0)
	for _, tc := range []struct {
		length time.Duration
		brief  bool
	}{
		{0, true},
		{5 * time.Second, true},
		{MeterBriefGap - time.Second, true},
		{MeterBriefGap, false},
		{90 * time.Minute, false},
	} {
		if got := (MeterGap{From: from, To: from.Add(tc.length), Reason: MeterStopShutdown}).Brief(); got != tc.brief {
			t.Errorf("a %s gap: Brief() = %v", tc.length, got)
		}
	}
}

// Every key of the meter file, literally: quota-glance and the status page read
// them. A meter with every field set writes each in this order, and one with
// nothing optional writes only the required keys, with empty arrays and
// objects rather than null.
func TestAPIMeterWireNames(t *testing.T) {
	seen := utc(2026, 10, 9, 11, 30)
	at := func(t time.Time) *time.Time { return &t }
	usage := []MeterUsage{{Model: "claude-haiku-5-5", Prompt: MeterPromptOver100K, Requests: 1, Failed: 2, Input: 3, Output: 4, CacheRead: 5, CacheWrite: 6}}
	meter := &APIMeter{
		Schema: MeterSchema, Since: utc(2026, 9, 20, 0, 0), StartedAt: utc(2026, 10, 9, 8, 0), FlushedAt: utc(2026, 10, 9, 11, 59),
		StoppedAt: at(utc(2026, 10, 9, 11, 59)), StopReason: MeterStopShutdown, Restarts: 3,
		Received: 10, Counted: 4, Foreign: 1,
		Rejected: 1, LastRejectedAt: at(seen), Dropped: 2, LastDroppedAt: at(seen), Unattributed: 1, LastUnattributedAt: at(seen),
		Gaps: []MeterGap{{From: utc(2026, 10, 9, 6, 0), To: utc(2026, 10, 9, 7, 30), Reason: MeterStopDisabled}},
		Organizations: map[string]MeterOrganization{"00000000-0000-4000-8000-00000000000a": {
			Since: utc(2026, 9, 20, 0, 0), UnlinkedAt: at(seen), LastSeenAt: at(seen), LastSuccessAt: at(seen),
			Refusals: 1, LastRefusalAt: at(seen), ClaudeCodeRefusals: 2, LastClaudeCodeRefusalAt: at(seen), LastOverflowAt: at(seen),
			Days:  []MeterBucket{{Start: utc(2026, 10, 9, 0, 0), Usage: usage}},
			Hours: []MeterBucket{{Start: utc(2026, 10, 9, 11, 0), Usage: usage}},
		}},
		Unlinked: []MeterUnlinked{{OrganizationID: "00000000-0000-4000-8000-00000000000b", FirstSeenAt: utc(2026, 10, 8, 10, 0), LastSeenAt: seen, Requests: 37}},
		Auths:    map[string]MeterAuth{"0123456789abcdef": {OrganizationID: "00000000-0000-4000-8000-00000000000a", SeenAt: seen}},
	}
	raw, err := json.Marshal(meter)
	if err != nil {
		t.Fatal(err)
	}
	entry := `[{"model":"claude-haiku-5-5","prompt":"over_100k","requests":1,"failed":2,"input":3,"output":4,"cache_read":5,"cache_write":6}]`
	want := `{"schema":1,"since":"2026-09-20T00:00:00Z","started_at":"2026-10-09T08:00:00Z","flushed_at":"2026-10-09T11:59:00Z",` +
		`"stopped_at":"2026-10-09T11:59:00Z","stop_reason":"shutdown","restarts":3,"received":10,"counted":4,"foreign":1,` +
		`"rejected":1,"last_rejected_at":"2026-10-09T11:30:00Z","dropped":2,"last_dropped_at":"2026-10-09T11:30:00Z",` +
		`"unattributed":1,"last_unattributed_at":"2026-10-09T11:30:00Z",` +
		`"gaps":[{"from":"2026-10-09T06:00:00Z","to":"2026-10-09T07:30:00Z","reason":"disabled"}],` +
		`"organizations":{"00000000-0000-4000-8000-00000000000a":{"since":"2026-09-20T00:00:00Z","unlinked_at":"2026-10-09T11:30:00Z",` +
		`"last_seen_at":"2026-10-09T11:30:00Z","last_success_at":"2026-10-09T11:30:00Z","refusals":1,"last_refusal_at":"2026-10-09T11:30:00Z",` +
		`"claude_code_refusals":2,"last_claude_code_refusal_at":"2026-10-09T11:30:00Z","last_overflow_at":"2026-10-09T11:30:00Z",` +
		`"days":[{"start":"2026-10-09T00:00:00Z","usage":` + entry + `}],"hours":[{"start":"2026-10-09T11:00:00Z","usage":` + entry + `}]}},` +
		`"unlinked":[{"organization_id":"00000000-0000-4000-8000-00000000000b","first_seen_at":"2026-10-08T10:00:00Z","last_seen_at":"2026-10-09T11:30:00Z","requests":37}],` +
		`"auths":{"0123456789abcdef":{"organization_id":"00000000-0000-4000-8000-00000000000a","seen_at":"2026-10-09T11:30:00Z"}}}`
	if string(raw) != want {
		t.Fatalf("meter =\n%s\nwant\n%s", raw, want)
	}
	path := filepath.Join(t.TempDir(), "snapshot.meter.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := LoadMeter(path); err != nil || !reflect.DeepEqual(loaded, meter) {
		t.Fatalf("round trip = %+v, %v", loaded, err)
	}

	start := utc(2026, 10, 9, 8, 0)
	bare, err := json.Marshal(APIMeter{
		Schema: MeterSchema, Since: start, StartedAt: start, FlushedAt: start, Gaps: []MeterGap{}, Unlinked: []MeterUnlinked{},
		Organizations: map[string]MeterOrganization{"00000000-0000-4000-8000-00000000000a": {Since: start, Days: []MeterBucket{}, Hours: []MeterBucket{}}},
		Auths:         map[string]MeterAuth{"0123456789abcdef": {SeenAt: start}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"schema":1,"since":"2026-10-09T08:00:00Z","started_at":"2026-10-09T08:00:00Z","flushed_at":"2026-10-09T08:00:00Z",` +
		`"received":0,"counted":0,"gaps":[],"organizations":{"00000000-0000-4000-8000-00000000000a":{"since":"2026-10-09T08:00:00Z","days":[],"hours":[]}},` +
		`"unlinked":[],"auths":{"0123456789abcdef":{"organization_id":"","seen_at":"2026-10-09T08:00:00Z"}}}`; string(bare) != want {
		t.Fatalf("bare meter =\n%s\nwant\n%s", bare, want)
	}
	if other, _ := json.Marshal(MeterUsage{Model: MeterOtherModel}); string(other) != `{"model":"(other)"}` {
		t.Fatalf("bare usage = %s", other)
	}
}

// The caps bound the file: a meter at every one of them, with the longest
// model names and every count at its maximum, still fits MaxMeterBytes, and
// LoadMeter reads it back whole.
func TestMaximalMeterFitsMaxMeterBytes(t *testing.T) {
	at := utc(2026, 10, 9, 12, 0)
	most := uint64(math.MaxUint64)
	stamp := func() *time.Time { v := at; return &v }
	orgID := func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", i) }
	meter := &APIMeter{
		Schema: MeterSchema, Since: at, StartedAt: at, FlushedAt: at,
		StoppedAt: stamp(), StopReason: MeterStopShutdown, Restarts: most,
		Received: most, Counted: most, Foreign: most,
		Rejected: most, LastRejectedAt: stamp(), Dropped: most, LastDroppedAt: stamp(),
		Unattributed: most, LastUnattributedAt: stamp(),
		Gaps: []MeterGap{}, Organizations: map[string]MeterOrganization{}, Unlinked: []MeterUnlinked{}, Auths: map[string]MeterAuth{},
	}
	for range MaxMeterGaps {
		meter.Gaps = append(meter.Gaps, MeterGap{From: at, To: at, Reason: MeterGapUncleanStop})
	}
	for i := range MaxMeterUnlinked {
		meter.Unlinked = append(meter.Unlinked, MeterUnlinked{OrganizationID: orgID(0x1000 + i), FirstSeenAt: at, LastSeenAt: at, Requests: most})
	}
	for i := range MaxMeterAuths {
		// The meter keeps auth indexes of up to 64 bytes.
		meter.Auths[fmt.Sprintf("%064x", i)] = MeterAuth{OrganizationID: orgID(i + 1), SeenAt: at}
	}

	// A bucket exists only with an entry, so every bucket of every
	// organization holds one, and the entries left over go one more to each
	// bucket until they run out.
	spare := MaxMeterUsageEntries - MaxMeterOrganizations*(MeterDays+1+MeterHours+1)
	entries := 0
	bucket := func(start time.Time) MeterBucket {
		n := 1
		if spare > 0 {
			n, spare = 2, spare-1
		}
		b := MeterBucket{Start: start}
		for k := range n {
			model := fmt.Sprintf("claude-%057d", k)
			b.Usage = append(b.Usage, MeterUsage{Model: model, Prompt: MeterPromptOver100K,
				Requests: most, Failed: most, Input: most, Output: most, CacheRead: most, CacheWrite: most})
		}
		entries += n
		return b
	}
	for i := range MaxMeterOrganizations {
		org := MeterOrganization{Since: at, UnlinkedAt: stamp(), LastSeenAt: stamp(), LastSuccessAt: stamp(),
			Refusals: most, LastRefusalAt: stamp(), ClaudeCodeRefusals: most, LastClaudeCodeRefusalAt: stamp(), LastOverflowAt: stamp()}
		for d := MeterDays; d >= 0; d-- {
			org.Days = append(org.Days, bucket(MeterDayStart(at).AddDate(0, 0, -d)))
		}
		for h := MeterHours; h >= 0; h-- {
			org.Hours = append(org.Hours, bucket(MeterHourStart(at).Add(-time.Duration(h)*time.Hour)))
		}
		meter.Organizations[orgID(i+1)] = org
	}
	if entries != MaxMeterUsageEntries || spare != 0 || len(meter.Organizations) != MaxMeterOrganizations || len(meter.Auths) != MaxMeterAuths {
		t.Fatalf("not a maximal meter: %d entries, %d spare, %d organizations, %d auths", entries, spare, len(meter.Organizations), len(meter.Auths))
	}
	if model := meter.Organizations[orgID(1)].Days[0].Usage[0].Model; len(model) != 64 || NormalizeMeterModel(model, "") != model {
		t.Fatalf("model %q is not a 64-character name the meter keeps", model)
	}

	raw, err := json.Marshal(meter)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxMeterBytes {
		t.Fatalf("a maximal meter is %d bytes, over MaxMeterBytes (%d)", len(raw), MaxMeterBytes)
	}
	t.Logf("a maximal meter is %d bytes of %d", len(raw), MaxMeterBytes)
	path := filepath.Join(t.TempDir(), "snapshot.meter.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMeter(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, meter) {
		t.Fatal("a maximal meter did not survive the round trip")
	}
}

// The meter lives in its own file. The snapshot's keys are pinned so that it
// can never gain the meter, or anything else, by accident: its released
// readers decode it whole, and save refuses it past MaxBytes.
func TestSnapshotHasNoMeterField(t *testing.T) {
	var keys []string
	snapshot := reflect.TypeFor[Snapshot]()
	for i := range snapshot.NumField() {
		name, _, _ := strings.Cut(snapshot.Field(i).Tag.Get("json"), ",")
		keys = append(keys, name)
	}
	want := []string{"schema", "written_at", "next_request", "provider_cooldown", "entries", "totals", "history"}
	if !slices.Equal(keys, want) {
		t.Fatalf("snapshot keys = %q, want %q", keys, want)
	}
}
