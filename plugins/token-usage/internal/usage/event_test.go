package usage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNarrowExactDecoder(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	raw := `{"Provider":"p","Model":"m","RequestedAt":"2026-09-09T00:00:00Z","Generate":false,"Failed":true,"Failure":{"StatusCode":503,"Body":"secret-canary"},"APIKey":"secret-canary","Source":"secret-canary","AuthIndex":"","SessionID":"secret-canary","ResponseHeaders":{"X-Key":["secret-canary"]},"Detail":{"InputTokens":9223372036854775807,"OutputTokens":9007199254740993}}`
	e, err := Decode([]byte(raw), now)
	if err != nil {
		t.Fatal(err)
	}
	if e.Tokens.InputTokens != 9223372036854775807 || e.Tokens.OutputTokens != 9007199254740993 || !e.Failed || e.Generate || e.Status != 503 || e.ExecutorType != "unknown" {
		t.Fatalf("event %+v", e)
	}
	serialized, _ := json.Marshal(e)
	if strings.Contains(string(serialized), "secret-canary") {
		t.Fatal("sensitive metadata decoded")
	}
	if e.Flags&FlagMissingTokens == 0 || e.Flags&FlagUnknownExecutor == 0 {
		t.Fatal("missing flags")
	}
	for _, value := range []string{"-1", "1.5", "9223372036854775808", `"1"`} {
		bad := strings.Replace(raw, "9223372036854775807", value, 1)
		if _, err := Decode([]byte(bad), now); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
func TestTimestampAndMetadataPolicies(t *testing.T) {
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for _, at := range []string{`null`, `"not-a-time"`, `"0001-01-01T00:00:00Z"`} {
		e, err := Decode([]byte(fmt.Sprintf(`{"Provider":"p","Model":"m","RequestedAt":%s}`, at)), now)
		if err != nil || e.RequestedAt != now || e.Flags&FlagTimeFallback == 0 || e.Flags&FlagMissingGenerate == 0 {
			t.Fatalf("fallback %+v %v", e, err)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"Provider":"p","Model":"m","RequestedAt":"2101-01-01T00:00:00Z"}`, `{"Provider":"p","Model":"m","RequestedAt":"2026-09-09T00:06:00Z"}`, `{"Provider":"p","Model":"m\n"}`, fmt.Sprintf(`{"Provider":"p","Model":%q}`, strings.Repeat("x", 513)), `{"Provider":"p","Model":"m","Failure":{"StatusCode":600}}`} {
		if _, err := Decode([]byte(raw), now); err == nil {
			t.Fatalf("accepted invalid usage %q", raw)
		}
	}
}
