package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestSidebarBrowser executes the shipped, CSP-protected page in a real browser against a
// synthetic loopback console. The server exists only in this test binary; the plugin build has no
// fixture endpoint or session setter. Opt in with CHAT_ARCHIVE_BROWSER_TEST=1 (make browser).
func TestSidebarBrowser(t *testing.T) {
	if os.Getenv("CHAT_ARCHIVE_BROWSER_TEST") != "1" {
		t.Skip("set CHAT_ARCHIVE_BROWSER_TEST=1 to run the agent-browser sidebar suite")
	}
	browser := os.Getenv("CHAT_ARCHIVE_AGENT_BROWSER")
	if browser == "" {
		t.Fatal("CHAT_ARCHIVE_AGENT_BROWSER is required (make browser-tools)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the browser runner")
	}
	var mu sync.Mutex
	scenario := "normal"
	statusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fixture":
			mu.Lock()
			scenario, statusCalls = r.URL.Query().Get("scenario"), 0
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><html><head><title>Synthetic console</title></head><body>Synthetic console fixture</body></html>"))
		case "/fixture/calls":
			mu.Lock()
			n := statusCalls
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]int{"status_calls": n})
		case resourcePath:
			resp := sidebarResponse()
			for k, v := range resp.Headers {
				w.Header()[k] = v
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(resp.Body)
		case statusPath:
			mu.Lock()
			statusCalls++
			current := scenario
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if current == "rejected" || r.Header.Get("Authorization") != "Bearer fixture-management-key" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"api_schema": 1, "plugin": ID, "version": Version, "state": "running", "host_schema": 10,
				"outbox":   map[string]any{"pending": 12, "oldest_pending_age_s": 75, "pending_bytes": 4096, "pressure": 0},
				"uploader": map[string]any{"consecutive_failures": 0, "epoch_seen": 1},
				"calls":    map[string]any{"before_auth": 31, "stream_chunk": 400},
				"dropped":  map[string]any{"body_budget": 2, "stopped": 0},
				"faults":   []string{"write_unavailable"}, "recovered_panics": map[string]any{},
				"prompt": "synthetic content that must not render",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cmd := exec.Command(node, filepath.Join("frontendtests", "sidebar.browser.cjs"))
	cmd.Env = append(os.Environ(), "CHAT_ARCHIVE_FIXTURE_URL="+server.URL)
	out, err := cmd.CombinedOutput()
	t.Log(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("browser runner failed: %v", err)
	}
}
