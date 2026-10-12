package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/protocol"
)

// quota-glance 0.7's two settings doors stay registered and answer 410
// settings_moved, never 404: CPA's own 404 for a path no plugin matched is
// empty, so this body is what proves CPA still dispatches the route. They
// answer before reading anything: no body, header or token changes the
// answer, and nothing is too large or malformed to get it.
func TestRetiredSettingsRoutesAnswerSettingsMoved(t *testing.T) {
	const moved = `{"error":"settings_moved","movedTo":"quota-cache"}`
	a := newTestAPI()
	for _, req := range []protocol.ManagementRequest{
		{Method: http.MethodPost, Path: "/v0/management/plugins/quota-glance/settings"},
		{Method: http.MethodPost, Path: "/v0/management/plugins/quota-glance/settings",
			Headers: http.Header{"Content-Type": {"text/plain"}, "Sec-Fetch-Site": {"cross-site"}}, Body: bytes.Repeat([]byte("{"), 1<<16)},
		{Method: http.MethodGet, Path: "/v0/resource/plugins/quota-glance/save-settings"},
		{Method: http.MethodGet, Path: "/v0/resource/plugins/quota-glance/save-settings",
			Headers: http.Header{"Authorization": {"Bearer wrong"}, "X-Quota-Glance-Settings": {"not base64"}}},
		{Method: http.MethodGet, Path: "/v0/resource/plugins/quota-glance/save-settings", Headers: bearer(testToken)},
	} {
		res := a.Handle(req, time.Now())
		if res.StatusCode != http.StatusGone || string(res.Body) != moved || res.Headers.Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(res.Headers.Get("Content-Type"), "application/json") {
			t.Errorf("%s %s: %d %s %v", req.Method, req.Path, res.StatusCode, res.Body, res.Headers)
		}
	}
	// A wrong token on the retired door is not a failed sign-in: it costs the
	// limiter nothing, so the next reader with the right token still gets in.
	for range failureLimit + 5 {
		a.Handle(protocol.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/quota-glance/save-settings",
			Headers: http.Header{"Authorization": {"Bearer wrong"}}}, time.Now())
	}
	if res := get(a, "/v0/resource/plugins/quota-glance/summary", bearer("wrong")); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token after the retired door's requests: %d", res.StatusCode)
	}

	// Disabled is still disabled.
	a.Disable()
	for _, req := range []protocol.ManagementRequest{
		{Method: http.MethodPost, Path: "/v0/management/plugins/quota-glance/settings"},
		{Method: http.MethodGet, Path: "/v0/resource/plugins/quota-glance/save-settings"},
	} {
		if res := a.Handle(req, time.Now()); res.StatusCode != http.StatusServiceUnavailable || string(res.Body) != `{"error":"disabled"}` {
			t.Errorf("disabled %s %s: %d %s", req.Method, req.Path, res.StatusCode, res.Body)
		}
	}
}
