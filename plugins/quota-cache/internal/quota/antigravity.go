package quota

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

// Google Antigravity.
//
// The requests are the ones CPA v8.0.77's own Antigravity executor makes, on
// the host it makes them to: every call goes to the daily Cloud Code host
// unless the credential's base_url names another (CPA
// internal/runtime/executor/antigravity_executor_request.go,
// resolveAntigravityRequestBaseURL), with CPA's fallback User-Agent unless the
// credential's user_agent names another (internal/misc/antigravity_version.go).
// The quota read and its parse follow the open-source reader in t3code
// (apps/server/src/provider/antigravityUsageLimits.ts): retrieveUserQuotaSummary
// first, and fetchAvailableModels only when the summary has nothing usable.
//
// Nothing here refreshes a token, retries, or calls a generate endpoint: CPA
// owns the credential and refreshes it, and a poll that fails waits out the
// cache's ordinary backoff.
const (
	antigravityDailyBase   = "https://daily-cloudcode-pa.googleapis.com"
	antigravitySummaryPath = "/v1internal:retrieveUserQuotaSummary"
	antigravityModelsPath  = "/v1internal:fetchAvailableModels"
	// antigravityLoadCodeAssistPath is read on the account-details schedule,
	// for the plan only.
	antigravityLoadCodeAssistPath = "/v1internal:loadCodeAssist"
	// antigravityUserAgent is CPA's fallback, the form it sends while the hub
	// manifest has not been fetched (misc.AntigravityUserAgent with
	// antigravityFallbackVersion 2.29.0 and its changelist).
	antigravityUserAgent = "antigravity/hub/2.29.0 (aidev_client; os_type=darwin; arch=arm64; cl=1013060670)"
	// maxAntigravityUserAgent bounds an operator's user_agent.
	maxAntigravityUserAgent = 256
	// maxAntigravityModels bounds the fallback's model list. A real one has
	// a couple of dozen models, which reduce to at most four windows.
	maxAntigravityModels = 4 * maxDetails
	// untouchedResetSlack is how close an untouched bucket's reset must sit
	// to a full window from now to be read as a rolling placeholder.
	untouchedResetSlack = time.Minute
)

// antigravityLoadCodeAssistBody is what CPA sends to loadCodeAssist.
var antigravityLoadCodeAssistBody = []byte(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)

// antigravityBucketID bounds a summary bucketId, which becomes a quota.windows
// key and, for an unmapped bucket, part of a raw: canonical key and its title.
// No "/" is allowed, so no bucket can collide with a "models/" fallback id.
var antigravityBucketID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// antigravityProject bounds a project id before it goes into a request body.
// Cloud project ids are letters, digits and hyphens; the dot and colon cover
// the legacy domain-scoped form.
var antigravityProject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// antigravityHost is a googleapis.com host name: lower-case labels only, so
// nothing that merely ends in the suffix after some other syntax passes.
var antigravityHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.googleapis\.com$`)

// projectID validates a project id from the auth JSON or the roster, and
// returns "" for one that does not look like a project id: the request is then
// sent with no project, as for a credential that names none.
func projectID(value string) string {
	value = strings.TrimSpace(value)
	if !antigravityProject.MatchString(value) {
		return ""
	}
	return value
}

// antigravityBaseURL honours an operator's base_url only when it is an https
// URL on a googleapis.com host, with no credentials, query or fragment and no
// port other than 443. Anything else gets the daily host CPA itself defaults
// to: the bearer token goes wherever this points, so it must never be a host
// outside Google's API domain. A trailing slash is dropped as CPA drops it.
func antigravityBaseURL(raw string) string {
	if raw == "" {
		return antigravityDailyBase
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return antigravityDailyBase
	}
	if port := u.Port(); port != "" && port != "443" {
		return antigravityDailyBase
	}
	host := strings.ToLower(u.Hostname())
	if !antigravityHost.MatchString(host) {
		return antigravityDailyBase
	}
	return "https://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
}

// antigravityUserAgentOf honours an operator's user_agent when it is printable
// ASCII and bounded, and otherwise sends CPA's fallback.
func antigravityUserAgentOf(raw string) string {
	if raw == "" || len(raw) > maxAntigravityUserAgent {
		return antigravityUserAgent
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return antigravityUserAgent
		}
	}
	return raw
}

// antigravityProjectBody is the summary and model-list request body: the
// project when one is known, else an empty object, as t3code sends.
func antigravityProjectBody(project string) []byte {
	if project == "" {
		return []byte(`{}`)
	}
	body, err := json.Marshal(map[string]string{"project": project})
	if err != nil {
		return []byte(`{}`)
	}
	return body
}

// antigravityRequest is one POST to a Cloud Code v1internal method.
func antigravityRequest(creds credentials, path string, body []byte) protocol.HostHTTPRequest {
	base := creds.baseURL
	if base == "" {
		base = antigravityDailyBase
	}
	userAgent := creds.userAgent
	if userAgent == "" {
		userAgent = antigravityUserAgent
	}
	return protocol.HostHTTPRequest{
		Method: "POST",
		URL:    base + path,
		Headers: map[string][]string{
			"Authorization": {"Bearer " + creds.accessToken},
			"Content-Type":  {"application/json"},
			"Accept":        {"application/json"},
			"User-Agent":    {userAgent},
		},
		Body: body,
	}
}

// antigravityWindowSeconds is the duration a summary bucket's window names.
func antigravityWindowSeconds(window string) (int64, bool) {
	switch window {
	case "5h":
		return sessionWindowSeconds, true
	case "weekly":
		return weeklyWindowSeconds, true
	}
	return 0, false
}

// antigravityUsed reads one quota reading — a summary bucket, or a model's
// quotaInfo/weeklyQuotaInfo — as used percent and reset instant.
//
// remainingFraction is the share left, 0..1. Google serves these responses as
// proto3 JSON, which omits a field whose value is zero, so an exhausted quota
// arrives as {"resetTime": "..."} with no remainingFraction at all. A reading
// with a resetTime and no fraction is therefore read as 0 remaining, 100%
// used. The evidence, checked 2026-11-19: the proto3 omission was observed on
// live accounts, where every exhausted gemini-* model answered only
// {"resetTime":"2026-10-31T00:30:21Z"} (gajae-code#7267), and the open-source
// readers that hit it settled on this reading (t3code#29565 for both the
// summary and model paths, pi-cliproxyapi-bridge#2, Antigravity-Manager's
// model_quota_percentage, ag-telemetry 2.0.2; proposed for CPA itself in
// CLIProxyAPI#7669). Two routers keep it "unknown" instead (OmniRoute#7138,
// 9router-go#449) so that a preflight check still lets requests through; a
// reader of quota windows has no such reason, and reading an exhausted pool
// as missing would hide exactly the account that has run out. A reading with
// neither field says nothing and is skipped (ok false).
//
// The result is rounded to nine decimal places so that floating-point noise
// (1-0.96 is not exactly 0.04) does not reach the snapshot or a threshold.
func antigravityUsed(quota map[string]any) (used float64, fraction float64, reset *time.Time, ok bool) {
	reset = timestamp(quota, "resetTime")
	fraction, has := numberField(quota, "remainingFraction")
	if !has {
		if reset == nil {
			return 0, 0, nil, false
		}
		fraction = 0
	}
	left := math.Max(0, math.Min(1, fraction))
	return math.Round((1-left)*100*1e9) / 1e9, fraction, reset, true
}

// antigravitySummary reads retrieveUserQuotaSummary into quota.windows, one
// window per usable bucket, keyed by its bucketId.
//
// Buckets come from groups[].buckets when any group has one, else from the
// top-level buckets. A disabled bucket, one without a valid bucketId, and one
// with neither a fraction nor a reset are skipped; a bucketId seen twice keeps
// its first reading. description is read only to recognise an untouched
// bucket, and displayName not at all.
func antigravitySummary(q *client.Quota, root map[string]any, now time.Time) {
	groups, _ := root["groups"].([]any)
	var buckets []any
	for _, item := range groups {
		if group, ok := item.(map[string]any); ok {
			if list, ok := group["buckets"].([]any); ok {
				buckets = append(buckets, list...)
			}
		}
	}
	if len(buckets) == 0 {
		buckets, _ = root["buckets"].([]any)
	}
	for _, item := range buckets {
		if q.Truncated {
			return
		}
		bucket, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if disabled := boolean(bucket, "disabled"); disabled != nil && *disabled {
			continue
		}
		id, _ := bucket["bucketId"].(string)
		if !antigravityBucketID.MatchString(id) {
			continue
		}
		if _, seen := q.Windows[id]; seen {
			continue
		}
		used, fraction, reset, ok := antigravityUsed(bucket)
		if !ok {
			continue
		}
		window := client.Window{UsedPercent: &used, ResetsAt: reset, Period: name(bucket, "window")}
		duration, known := antigravityWindowSeconds(stringField(bucket, "window"))
		if known {
			window.DurationSeconds = &duration
		}
		// An untouched bucket reports a rolling reset one full window from
		// now, which moves forward on every poll. Recording it would rewrite
		// the snapshot each time and show a reset that never arrives, so an
		// untouched bucket records none.
		if known && reset != nil && fraction == 1 && stringField(bucket, "description") == "" {
			if offset := reset.Sub(now.Add(time.Duration(duration) * time.Second)); offset <= untouchedResetSlack && offset >= -untouchedResetSlack {
				window.ResetsAt = nil
			}
		}
		addWindow(q, id, window)
	}
}

// antigravityModelFamily is a fetchAvailableModels model's quota family:
// "gemini", or "shared" for the pool Claude and GPT draw on, or "" for a model
// in neither, which is skipped.
func antigravityModelFamily(id, provider string) string {
	switch {
	case provider == "MODEL_PROVIDER_GOOGLE" || strings.HasPrefix(id, "gemini-"):
		return "gemini"
	case provider == "MODEL_PROVIDER_ANTHROPIC" || strings.HasPrefix(id, "claude-"),
		provider == "MODEL_PROVIDER_OPENAI" || strings.HasPrefix(id, "gpt-"):
		return client.AntigravityModelShared
	}
	return ""
}

// antigravityModels reads a fetchAvailableModels answer into at most four
// windows: models/<family>-weekly from weeklyQuotaInfo, a seven-day window,
// and models/<family>-quota from quotaInfo, which declares no window length.
// Models of one family share their pools, so each window keeps the family's
// most-used reading; ties keep the first in model-id order. No cadence is
// invented for quotaInfo: it carries no duration and so maps to raw:.
func antigravityModels(q *client.Quota, root map[string]any) {
	models, _ := root["models"].(map[string]any)
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	best := map[string]client.Window{}
	for i, id := range ids {
		if i >= maxAntigravityModels {
			q.Truncated = true
			break
		}
		model, ok := models[id].(map[string]any)
		if !ok {
			continue
		}
		family := antigravityModelFamily(id, stringField(model, "modelProvider"))
		if family == "" {
			continue
		}
		for _, slot := range []struct{ field, suffix string }{{"quotaInfo", "quota"}, {"weeklyQuotaInfo", "weekly"}} {
			used, _, reset, ok := antigravityUsed(object(model, slot.field))
			if !ok {
				continue
			}
			key := "models/" + family + "-" + slot.suffix
			if previous, seen := best[key]; seen && *previous.UsedPercent >= used {
				continue
			}
			window := client.Window{UsedPercent: &used, ResetsAt: reset}
			if slot.suffix == "weekly" {
				duration := int64(weeklyWindowSeconds)
				window.DurationSeconds, window.Period = &duration, "weekly"
			}
			best[key] = window
		}
	}
	keys := make([]string, 0, len(best))
	for key := range best {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		addWindow(q, key, best[key])
	}
}

// antigravityModelsFallback asks fetchAvailableModels and reads its answer into
// q. Every failure — transport, non-2xx, oversize, unreadable — leaves q as
// it was: the summary already answered, so the poll is judged by it.
func antigravityModelsFallback(ctx context.Context, doer Doer, creds credentials, q *client.Quota) {
	response, err := doer.HTTPDo(ctx, antigravityRequest(creds, antigravityModelsPath, antigravityProjectBody(creds.projectID)))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || len(response.Body) > maxResponseBytes {
		return
	}
	root, err := decodeObject(response.Body)
	if err != nil {
		return
	}
	antigravityModels(q, root)
}

// antigravityFamily names the quota family a collected window belongs to: its
// canonical model token ("" for the default Gemini family) and the display
// name its title carries. ok is false for a window in no known family.
//
// Summary buckets are told apart by bucketId prefix; the Gemini families Google
// meters separately are matched before the default one, so a gemini-hydrogen-
// bucket can never be read as the default Gemini weekly.
func antigravityFamily(id string) (model, label string, ok bool) {
	if family, fallback := strings.CutPrefix(id, "models/"); fallback {
		switch {
		case strings.HasPrefix(family, "gemini-"):
			return "", "Gemini", true
		case strings.HasPrefix(family, client.AntigravityModelShared+"-"):
			return client.AntigravityModelShared, "Claude & GPT", true
		}
		return "", "", false
	}
	switch {
	case strings.HasPrefix(id, "3p-"):
		return client.AntigravityModelShared, "Claude & GPT", true
	case strings.HasPrefix(id, "gemini-argon-"):
		return client.AntigravityModelArgon, "Gemini Argon", true
	case strings.HasPrefix(id, "gemini-carbon-"):
		return client.AntigravityModelCarbon, "Gemini Carbon", true
	case strings.HasPrefix(id, "gemini-hydrogen-"):
		return client.AntigravityModelHydrogen, "Gemini Hydrogen", true
	case strings.HasPrefix(id, "gemini-"):
		return "", "Gemini", true
	}
	return "", "", false
}

// mapAntigravity keys a window on its family and its declared duration, never
// on the id's suffix, as mapCodex does. The default Gemini family is the
// account-wide session/weekly; every other family is model_session/
// model_weekly carrying its token as Model. A window in no known family, or
// with no declared five-hour or weekly duration, is raw:.
func mapAntigravity(id string, duration int64, hasDuration bool) canonical {
	model, label, ok := antigravityFamily(id)
	if !ok || !hasDuration {
		return raw(client.ProviderAntigravity, id)
	}
	switch {
	case duration == sessionWindowSeconds && model == "":
		return canonical{key: client.WindowSession, title: "Session (" + label + ")"}
	case duration == weeklyWindowSeconds && model == "":
		return canonical{key: client.WindowWeekly, title: "Weekly (" + label + ")"}
	case duration == sessionWindowSeconds:
		return canonical{key: client.WindowModelSession, model: model, title: "Session (" + label + ")"}
	case duration == weeklyWindowSeconds:
		return canonical{key: client.WindowModelWeekly, model: model, title: "Weekly (" + label + ")"}
	}
	return raw(client.ProviderAntigravity, id)
}

// antigravityDefaultIDs are the default Gemini family's own window ids, in the
// summary and in the model-list fallback.
var antigravityDefaultIDs = map[string]bool{
	"gemini-5h":            true,
	"gemini-weekly":        true,
	"models/gemini-weekly": true,
}

// sortWindowIDs orders collected window ids for the canonical projection,
// whose first id wins a canonical key that several ids reduce to. It is plain
// string order, except that Antigravity's exact default Gemini ids go first: a
// gemini-<family>- bucket Google adds later maps to the default family too, and
// "gemini-neon-weekly" sorts before "gemini-weekly", so in plain order the new
// family would take the account-wide weekly and Entry.Percent while the real
// one was demoted to raw:. A stranger still serves as the default when it is
// the only one there is.
func sortWindowIDs(provider string, ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		if provider == client.ProviderAntigravity {
			if a, b := antigravityDefaultIDs[ids[i]], antigravityDefaultIDs[ids[j]]; a != b {
				return a
			}
		}
		return ids[i] < ids[j]
	})
}

// parseAntigravity is the compatibility observation: the default Gemini
// family's weekly window, and nothing else. When several windows map there,
// it is the first in sortWindowIDs order, the one canonicalWindows keeps as
// weekly. An
// account with no such window — one whose summary carries only the Claude &
// GPT pool, say — has none, and ErrNoWeeklyWindow leaves its windows published
// with no entry-level observation, as for a Codex short-window-only account.
func parseAntigravity(q *client.Quota) (Observation, error) {
	if q == nil {
		return Observation{}, ErrNoWeeklyWindow
	}
	ids := make([]string, 0, len(q.Windows))
	for id := range q.Windows {
		ids = append(ids, id)
	}
	sortWindowIDs(client.ProviderAntigravity, ids)
	for _, id := range ids {
		w := q.Windows[id]
		if w.UsedPercent == nil || w.DurationSeconds == nil {
			continue
		}
		if mapAntigravity(id, *w.DurationSeconds, true).key != client.WindowWeekly {
			continue
		}
		observation := Observation{Percent: clampPercent(*w.UsedPercent)}
		if w.ResetsAt != nil {
			observation.ResetAt = *w.ResetsAt
		}
		return observation, nil
	}
	return Observation{}, ErrNoWeeklyWindow
}

// antigravityPlanName reads the account's tier name from loadCodeAssist: the
// paid tier's when it has a valid one, else the current tier's. Tier ids,
// credits and project ids in the same answer are not kept. answered is false
// only when the request failed.
func antigravityPlanName(ctx context.Context, doer Doer, creds credentials) (plan string, answered bool) {
	root, ok := readAccount(ctx, doer, antigravityRequest(creds, antigravityLoadCodeAssistPath, antigravityLoadCodeAssistBody))
	if !ok {
		return "", false
	}
	if plan := name(object(root, "paidTier"), "name"); plan != "" {
		return plan, true
	}
	return name(object(root, "currentTier"), "name"), true
}
