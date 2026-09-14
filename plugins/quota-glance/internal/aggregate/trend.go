package aggregate

import "time"

// Trend needs history, which quota-cache does not keep: it holds the current
// observation, not a series. The store maintains a short ring and passes it in
// here, so this stays as pure as the rest of the package.
const (
	trendLookback  = time.Hour
	trendMinSpan   = 30 * time.Minute
	trendThreshold = 0.02
)

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// observation is one member's current remaining fraction. Members arrive in
// catalog order, and the means below are accumulated in that order so float
// addition cannot make the result depend on map iteration.
type observation struct {
	authIndex string
	remaining float64
}

// trendOf compares the row's current value against its value an hour ago.
//
// Both means are taken over the SAME members — those that have history. Taking
// the current mean over everyone and the historical mean over whoever happened
// to have samples compares two different populations, and reports movement for
// a row where nothing moved.
//
// A short or sparse history reports unknown rather than guessing; the client
// hides the arrow entirely in that case, which is better than an arrow that
// flips on noise.
func trendOf(samples []Sample, current []observation, rowID string, now time.Time) string {
	members := make(map[string]bool, len(current))
	for _, o := range current {
		members[o.authIndex] = true
	}
	target := now.Add(-trendLookback)
	nearest := map[string]Sample{}
	var oldest, newest time.Time
	count := 0
	for _, s := range samples {
		if s.WindowKey != rowID || !members[s.AuthIndex] || s.At.IsZero() || s.At.After(now) {
			continue
		}
		count++
		if oldest.IsZero() || s.At.Before(oldest) {
			oldest = s.At
		}
		if newest.IsZero() || s.At.After(newest) {
			newest = s.At
		}
		if best, ok := nearest[s.AuthIndex]; !ok || absDuration(s.At.Sub(target)) < absDuration(best.At.Sub(target)) {
			nearest[s.AuthIndex] = s
		}
	}
	if count < 2 || newest.Sub(oldest) < trendMinSpan || len(nearest) == 0 {
		return TrendUnknown
	}
	var nowSum, pastSum float64
	compared := 0
	for _, o := range current {
		sample, ok := nearest[o.authIndex]
		if !ok {
			continue
		}
		nowSum += o.remaining
		pastSum += sample.Remaining
		compared++
	}
	if compared == 0 {
		return TrendUnknown
	}
	switch delta := (nowSum - pastSum) / float64(compared); {
	case delta > trendThreshold:
		return TrendUp
	case delta < -trendThreshold:
		return TrendDown
	}
	return TrendFlat
}

// SamplesFrom projects a built document back into trend samples. Taking them
// from the document rather than the snapshot means a sample always matches the
// row it will later be compared against, including per-model rows.
func SamplesFrom(doc Document, at time.Time) []Sample {
	samples := []Sample{}
	for _, provider := range doc.Providers {
		for _, row := range provider.Rows {
			for _, entry := range row.Entries {
				samples = append(samples, Sample{
					AuthIndex: entry.CredentialID,
					WindowKey: row.RowID,
					At:        at,
					Remaining: entry.RemainingFraction,
				})
			}
		}
	}
	return samples
}
