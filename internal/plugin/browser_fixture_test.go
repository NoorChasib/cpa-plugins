package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-token-usage/internal/store"
)

// TestSidebarBrowser executes the shipped, CSP-protected page in a real browser.
// This synthetic loopback server exists only in the Go test binary. It never
// exposes a fixture endpoint, session setter or fake data in the plug-in build.
// Opt in with TOKEN_USAGE_BROWSER_TEST=1 and install agent-browser separately.
func TestSidebarBrowser(t *testing.T) {
	if os.Getenv("TOKEN_USAGE_BROWSER_TEST") != "1" {
		t.Skip("set TOKEN_USAGE_BROWSER_TEST=1 to run the installed agent-browser fixture suite")
	}
	browser := os.Getenv("TOKEN_USAGE_AGENT_BROWSER")
	if browser == "" {
		var err error
		browser, err = exec.LookPath("agent-browser")
		if err != nil {
			t.Fatal("agent-browser not found: install the test tool or set TOKEN_USAGE_AGENT_BROWSER")
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node.js 22.13+ is required for the browser fixture runner")
	}
	var mu sync.Mutex
	scenario := "normal"
	requests := []map[string]any{}
	streamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture" {
			mu.Lock()
			scenario = r.URL.Query().Get("scenario")
			requests = []map[string]any{}
			streamClosed = make(chan struct{})
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><html><head><title>Local browser fixture</title></head><body>Local synthetic console fixture</body></html>"))
			return
		}
		if r.URL.Path == "/fixture/frame" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><title>Synthetic console sidebar</title></head><body><iframe title="Token Usage" src="/v0/resource/plugins/token-usage/status"></iframe></body></html>`))
			return
		}
		if r.URL.Path == "/fixture/stream-closed" {
			mu.Lock()
			closed := streamClosed
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-closed:
				_, _ = w.Write([]byte(`{"closed":true}`))
			case <-timer.C:
				w.WriteHeader(http.StatusGatewayTimeout)
				_, _ = w.Write([]byte(`{"closed":false}`))
			case <-r.Context().Done():
			}
			return
		}
		if r.URL.Path == "/fixture/events" {
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(requests)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v0/resource/plugins/token-usage/status") {
			rendered := sidebarResponse()
			for name, values := range rendered.Headers {
				w.Header()[name] = values
			}
			w.WriteHeader(rendered.StatusCode)
			_, _ = w.Write(rendered.Body)
			return
		}
		if !strings.Contains(r.URL.Path, "/v0/management/plugins/token-usage/") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		mode := scenario
		closed := streamClosed
		requestIndex := len(requests)
		expectedKey := "fixture-management-key"
		if mode == "keyspaces" {
			expectedKey = "key with space"
		} else if mode == "keylatin1" {
			// Fetch sends a ByteString's Latin-1 code point as one header byte,
			// not Go's UTF-8 encoding of the corresponding Unicode character.
			expectedKey = "fixture-key-caf\xe9"
		}
		authorized := r.Header.Get("Authorization") == "Bearer "+expectedKey
		record := map[string]any{"path": r.URL.Path, "query": r.URL.Query(), "authorized": authorized, "method": r.Method}
		requests = append(requests, record)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !authorized {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		endpoint := filepath.Base(r.URL.Path)
		if mode == "stream401" || mode == "stream403" || mode == "streamwrongmime" {
			code := http.StatusOK
			if mode == "streamwrongmime" {
				w.Header().Set("Content-Type", "text/plain")
			} else {
				code, _ = strconv.Atoi(strings.TrimPrefix(mode, "stream"))
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"api_schema":1,`))
			w.(http.Flusher).Flush()
			// Keep the body open until the browser actually cancels the request.
			// The acknowledgement endpoint waits for this real network close.
			<-r.Context().Done()
			close(closed)
			return
		}
		if mode == "401" || mode == "403" || (mode == "summary401" && endpoint == "summary") {
			code, _ := strconv.Atoi(mode)
			if code == 0 {
				code = 401
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		if mode == "redirect" {
			http.Redirect(w, r, "/fixture/login", http.StatusFound)
			return
		}
		if mode == "html" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><script>window.fixtureXSS=true</script>Sign in</html>`))
			return
		}
		if mode == "wrongmime" {
			// Everything except MIME remains a valid status response, so this
			// scenario cannot pass merely because JSON/schema decoding failed.
			w.Header().Set("Content-Type", "text/plain")
		}
		if mode == "invalidjson" {
			_, _ = w.Write([]byte(`{"api_schema":`))
			return
		}
		if mode == "timeout" {
			<-r.Context().Done()
			return
		}
		cov, advancing := browserAdvancingCoverage(mode, requestIndex)
		if !advancing {
			cov = browserCoverage()
		}
		mu.Lock()
		record["coverage_from"] = cov["from"]
		record["coverage_to"] = cov["to"]
		record["retention_floor"] = cov["retention_floor"]
		mu.Unlock()
		if advancing && endpoint != "status" {
			from, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
			floor, _ := time.Parse(time.RFC3339Nano, cov["from"])
			if err != nil || from.Before(floor) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "outside_retained_coverage", "coverage": cov})
				return
			}
		}
		if mode == "emptycoverage" {
			cov["from"] = cov["to"]
		}
		if mode == "clipped" {
			cov["from"] = "2026-09-09T01:02:03.123456789Z"
		}
		if (mode == "416" || mode == "rolling") && endpoint != "status" {
			cov["from"] = "2026-09-09T02:03:04.987654321Z"
			from, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
			floor, _ := time.Parse(time.RFC3339Nano, cov["from"])
			if mode == "416" || from.Before(floor) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "outside_retained_coverage", "coverage": cov})
				return
			}
		}
		health := browserCollection()
		if mode == "degraded" || mode == "stopped" {
			health["state"] = mode
			health["degraded"] = true
			health["reason"] = "queue_full"
		}
		value := map[string]any{"api_schema": 1, "source": "cpa_reported", "upstream_completeness": "unknown", "state": health["state"], "collection": health, "coverage": cov}
		if mode == "schema" {
			value["api_schema"] = 2
		}
		if mode == "source" {
			value["source"] = "untrusted"
		}
		if mode == "unavailable" {
			value = map[string]any{"api_schema": 1, "source": "cpa_reported", "upstream_completeness": "unknown", "error": "storage_unavailable", "state": "unavailable", "collection": map[string]any{"state": "unavailable", "reason": "storage_initialization_failed"}}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		if endpoint != "status" {
			value["interval"] = map[string]string{"from": r.URL.Query().Get("from"), "to": r.URL.Query().Get("to")}
			value["aggregation"] = "provider_model_combined_executors"
			value["auxiliary_counters_additive"] = false
			totals := browserTotals(mode == "empty")
			if mode == "badcounter" {
				totals["observed_events"] = float64(9007199254740992)
			}
			if endpoint == "summary" {
				value["totals"] = totals
			} else {
				rows := []map[string]any{}
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				if mode != "empty" {
					for i := offset; i < min(offset+25, 27); i++ {
						row := browserTotals(false)
						row["provider"] = "provider-a"
						row["model"] = fmt.Sprintf("model-%02d", i)
						if i == 0 {
							row["model"] = `<img src=x onerror="window.fixtureXSS=true"> & model`
						}
						rows = append(rows, row)
					}
				}
				value["models"] = rows
				value["offset"] = offset
				value["limit"] = 25
				value["has_more"] = mode != "empty" && offset == 0
			}
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	defer server.CloseClientConnections()
	path, err := filepath.Abs("frontendtests/sidebar.browser.cjs")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, path)
	cmd.Env = append(os.Environ(), "TOKEN_USAGE_FIXTURE_URL="+server.URL, "TOKEN_USAGE_AGENT_BROWSER="+browser)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("browser suite: %v", err)
	}
}

// Use the real coverage constructor with a clock advancing 2 ms per request,
// including status -> summary -> models. The old exact-floor preset therefore
// fails here exactly as it does against a mature collector, without wall sleeps.
func browserAdvancingCoverage(mode string, requestIndex int) (map[string]string, bool) {
	hours, ok := map[string]int{"mature720h": 720, "mature168h": 168, "mature24h": 24, "mature12h": 12, "young24h": 24, "young-equality": 720}[mode]
	if !ok {
		return nil, false
	}
	retention := time.Duration(hours) * time.Hour
	base := time.Date(2026, 9, 9, 12, 0, 0, 123456789, time.UTC)
	start := base.Add(-2 * retention)
	if mode == "young24h" {
		start = base.Add(-10 * time.Second)
	} else if mode == "young-equality" {
		start = base.Add(-24 * time.Hour)
	}
	cov := store.MakeCoverage(start.UnixNano(), base.Add(-retention).UnixNano(), base.Add(time.Duration(requestIndex)*2*time.Millisecond), retention)
	return map[string]string{"from": cov.From.Format(time.RFC3339Nano), "to": cov.To.Format(time.RFC3339Nano), "retention_floor": cov.RetentionFloor.Format(time.RFC3339Nano), "raw_retention": cov.RawRetention, "resolution": cov.Resolution, "upstream_completeness": cov.Completeness}, true
}

func browserCoverage() map[string]string {
	return map[string]string{"from": "2026-08-10T12:00:00.123456789Z", "to": "2026-09-09T12:00:00.123456789Z", "retention_floor": "2026-08-10T12:00:00.123456789Z", "raw_retention": "720h0m0s", "resolution": "raw", "upstream_completeness": "unknown"}
}
func browserCollection() map[string]any {
	return map[string]any{"state": "running", "reason": "", "degraded": false, "last_persisted_at": "2026-09-09T11:59:59.123456789Z", "disk_bytes": "9007199254740993", "previous_unclean_runs": "0", "queue_depth": 0, "retention_cleanup_pending": false, "last_failure_reason": "", "active_faults": []string{}, "upstream_completeness": "unknown", "diagnostics": map[string]string{"committed_events": "9007199254740993", "dropped_queue_full": "0"}}
}
func browserTotals(empty bool) map[string]any {
	input, events := "9007199254740993", "9007199254740995"
	if empty {
		input, events = "0", "0"
	}
	return map[string]any{"observed_events": events, "successful_events": events, "failed_events": "0", "anomalous_events": "0", "reported_tokens": map[string]string{"input_tokens": input, "output_tokens": "200", "total_tokens": "42", "reasoning_tokens": "18", "cached_tokens": "16", "cache_read_tokens": "12", "cache_creation_tokens": "4"}, "executor_types": []string{"executor-a", "executor-b"}, "executor_types_truncated": true}
}
