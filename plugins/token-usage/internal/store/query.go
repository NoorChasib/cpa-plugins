package store

import (
	"context"
	"database/sql"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/protocol"
)

type Filter struct {
	From, To        time.Time
	Provider, Model string
	Limit, Offset   int
	Models          bool
}
type Coverage struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	RetentionFloor time.Time `json:"retention_floor"`
	RawRetention   string    `json:"raw_retention"`
	Resolution     string    `json:"resolution"`
	Completeness   string    `json:"upstream_completeness"`
}
type Totals struct {
	Observed           string            `json:"observed_events"`
	Successful         string            `json:"successful_events"`
	Failed             string            `json:"failed_events"`
	Anomalous          string            `json:"anomalous_events"`
	Tokens             map[string]string `json:"reported_tokens"`
	Executors          []string          `json:"executor_types"`
	ExecutorsTruncated bool              `json:"executor_types_truncated"`
}
type ModelRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Totals
}
type QueryResult struct {
	Coverage Coverage
	Totals   Totals
	Models   []ModelRow
	HasMore  bool
}
type accumulator struct {
	tokens                      [7]big.Int
	observed, failed, anomalous uint64
	executors                   map[string]struct{}
	truncated                   bool
}

var names = []string{"input_tokens", "output_tokens", "total_tokens", "reasoning_tokens", "cached_tokens", "cache_read_tokens", "cache_creation_tokens"}

func (a *accumulator) add(d protocol.UsageDetail, failed bool, flags uint64, executor string) {
	a.observed++
	if failed {
		a.failed++
	}
	if flags != 0 {
		a.anomalous++
	}
	values := []int64{d.InputTokens, d.OutputTokens, d.TotalTokens, d.ReasoningTokens, d.CachedTokens, d.CacheReadTokens, d.CacheCreationTokens}
	var n big.Int
	for i, v := range values {
		n.SetInt64(v)
		a.tokens[i].Add(&a.tokens[i], &n)
	}
	if a.executors == nil {
		a.executors = make(map[string]struct{})
	}
	if _, ok := a.executors[executor]; !ok {
		if len(a.executors) < 32 {
			a.executors[executor] = struct{}{}
		} else {
			a.truncated = true
		}
	}
}
func (a *accumulator) result() Totals {
	t := Totals{Observed: strconv.FormatUint(a.observed, 10), Successful: strconv.FormatUint(a.observed-a.failed, 10), Failed: strconv.FormatUint(a.failed, 10), Anomalous: strconv.FormatUint(a.anomalous, 10), Tokens: make(map[string]string), Executors: []string{}, ExecutorsTruncated: a.truncated}
	for i, n := range names {
		t.Tokens[n] = a.tokens[i].String()
	}
	for e := range a.executors {
		t.Executors = append(t.Executors, e)
	}
	sort.Strings(t.Executors)
	return t
}
func MakeCoverage(start, floor int64, now time.Time, retention time.Duration) Coverage {
	cutoff := now.Add(-retention).UnixNano()
	if cutoff > floor {
		floor = cutoff
	}
	if floor > start {
		start = floor
	}
	return Coverage{From: time.Unix(0, start).UTC(), To: now.UTC(), RetentionFloor: time.Unix(0, floor).UTC(), RawRetention: retention.String(), Resolution: "raw", Completeness: "unknown"}
}
func (s *Store) Query(ctx context.Context, f Filter, now time.Time) (QueryResult, error) {
	var result QueryResult
	tx, err := s.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	start, err := integer(ctx, tx, "coverage_start")
	if err != nil {
		return result, err
	}
	floor, err := integer(ctx, tx, "retention_floor")
	if err != nil {
		return result, err
	}
	result.Coverage = MakeCoverage(start, floor, now, s.cfg.RawRetention)
	if f.From.Before(result.Coverage.From) {
		return result, ErrCoverage
	}
	query := `SELECT provider,model,executor_type,failed,flags,input_tokens,output_tokens,total_tokens,reasoning_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens FROM usage_events WHERE requested_at>=? AND requested_at<?`
	args := []any{f.From.UnixNano(), f.To.UnixNano()}
	if f.Provider != "" {
		query += " AND provider=?"
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		query += " AND model=?"
		args = append(args, f.Model)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	var total accumulator
	type key struct{ provider, model string }
	groups := make(map[key]*accumulator)
	for rows.Next() {
		var provider, model, executor string
		var failed bool
		var flags uint64
		var d protocol.UsageDetail
		if err = rows.Scan(&provider, &model, &executor, &failed, &flags, &d.InputTokens, &d.OutputTokens, &d.TotalTokens, &d.ReasoningTokens, &d.CachedTokens, &d.CacheReadTokens, &d.CacheCreationTokens); err != nil {
			return result, err
		}
		total.add(d, failed, flags, executor)
		if f.Models {
			k := key{provider, model}
			a := groups[k]
			if a == nil {
				if len(groups) >= 100000 {
					return result, ErrUnavailable
				}
				a = &accumulator{}
				groups[k] = a
			}
			a.add(d, failed, flags, executor)
		}
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	result.Totals = total.result()
	result.Models = []ModelRow{}
	if f.Models {
		keys := make([]key, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].provider == keys[j].provider {
				return keys[i].model < keys[j].model
			}
			return keys[i].provider < keys[j].provider
		})
		end := f.Offset + f.Limit
		if end > len(keys) {
			end = len(keys)
		}
		result.HasMore = end < len(keys)
		for i := f.Offset; i < end; i++ {
			k := keys[i]
			result.Models = append(result.Models, ModelRow{Provider: k.provider, Model: k.model, Totals: groups[k].result()})
		}
	}
	return result, nil
}
