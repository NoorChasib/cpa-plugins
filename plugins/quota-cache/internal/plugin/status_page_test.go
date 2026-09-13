package plugin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/protocol"
)

func TestPublicSidebarIsStaticAndDoesNotStartPolling(t *testing.T) {
	host := &testHost{}
	p := New(host)
	defer p.Shutdown()
	for _, query := range []string{"", "?refresh=true&token=private-canary"} {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/quota-cache/status", Body: []byte(query)})
		result, err := p.Handle(protocol.MethodManagementHandle, raw)
		if err != nil {
			t.Fatal(err)
		}
		response := result.(protocol.ManagementResponse)
		if response.StatusCode != 200 || string(response.Body) != sidebarDocument || strings.Contains(string(response.Body), "private-canary") {
			t.Fatal("resource response depends on request data")
		}
		if !strings.Contains(response.Headers.Get("Content-Security-Policy"), sidebarHash(browserAuthScript+sidebarScript)) || strings.Contains(response.Headers.Get("Content-Security-Policy"), "unsafe-inline") {
			t.Fatal("missing strict script hash")
		}
	}
	if p.cache != nil || host.lists.Load() != 0 {
		t.Fatal("sidebar started polling")
	}
	for _, path := range []string{"/v0/resource/plugins/quota-cache/data", "/v0/resource/plugins/quota-cache/status/html"} {
		raw, _ := json.Marshal(protocol.ManagementRequest{Method: "GET", Path: path})
		result, _ := p.Handle(protocol.MethodManagementHandle, raw)
		if result.(protocol.ManagementResponse).StatusCode != 404 {
			t.Fatal("resource route exposed quota data")
		}
	}
}
