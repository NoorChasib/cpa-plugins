package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestFieldsSkipsLargeValues(t *testing.T) {
	big := strings.Repeat("QUFB", 1<<18)
	raw := []byte(`{"A":"x\"y","B":{"n":[1,{"s":"}"}]},"Body":"` + big + `","C":null,"D":true}`)
	var keys []string
	err := Fields(raw, func(k, v []byte) error {
		keys = append(keys, string(k))
		if string(k) == "Body" && len(v) != len(big)+2 {
			t.Fatalf("Body span = %d", len(v))
		}
		return nil
	})
	if err != nil || strings.Join(keys, ",") != "A,B,Body,C,D" {
		t.Fatalf("keys = %v err %v", keys, err)
	}
	if err := Fields([]byte(`{"A":"unterminated}`), func(k, v []byte) error { return nil }); err == nil {
		t.Fatal("malformed body accepted")
	}
}

func TestDecodeBase64AndUnquote(t *testing.T) {
	payload := []byte("synthetic body \x00\xff")
	val, _ := json.Marshal(payload)
	got, err := DecodeBase64(val)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("got %q err %v", got, err)
	}
	if b, _ := DecodeBase64([]byte("null")); b != nil {
		t.Fatal("null decoded")
	}
	s, _ := json.Marshal("a\"b\\c\né\U0001F600<>&")
	out, err := UnquoteInto(nil, s)
	if err != nil || string(out) != "a\"b\\c\né\U0001F600<>&" {
		t.Fatalf("unquote = %q err %v", out, err)
	}
}

func TestScanResponseSkipsRequestHeaders(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte(`{"id":"x"}`))
	raw := []byte(`{"RequestID":"r1","RequestHeaders":{"X-Api-Key":["upstream-secret-canary"]},"ResponseHeaders":{"Content-Type":["application/json"]},` +
		`"OriginalRequest":"` + body + `","RequestBody":null,"Body":"` + body + `","StatusCode":200,"Metadata":{"request_path":"/v1/messages","generate":true,"other":{"deep":[1]}}}`)
	r, sp, err := ScanResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := json.Marshal(r)
	if strings.Contains(string(enc), "upstream-secret-canary") {
		t.Fatal("RequestHeaders materialized")
	}
	if r.RequestID != "r1" || r.StatusCode != 200 || r.OriginalAbsent || !r.Meta.Present || r.Meta.RequestPath != "/v1/messages" || r.Meta.Generate == nil || !*r.Meta.Generate {
		t.Fatalf("decoded = %+v", r)
	}
	b, _ := DecodeBase64(sp.Body)
	if string(b) != `{"id":"x"}` {
		t.Fatalf("body = %s", b)
	}
}

func TestDecodeUsageZeroesScratch(t *testing.T) {
	key := "sk-cpa-synthetic-canary-0123456789"
	src := "user@example.invalid"
	raw := []byte(`{"RequestID":"u1","Provider":"claude","APIKey":"` + key + `","Source":"` + src + `","AuthID":"auth/file.json","AuthIndex":"abcd",` +
		`"Latency":1500000000,"TTFT":250000000,"Failed":true,"Failure":{"StatusCode":500,"Body":"oops"},"Detail":{"InputTokens":5},"ResponseHeaders":{"X-Request-Id":["q"]}}`)
	var seen UsageSecrets
	var keep []byte
	r, err := DecodeUsage(raw, func(u *Usage, s UsageSecrets) {
		seen = s
		keep = s.APIKey
		if string(s.APIKey) != key || string(s.Source) != src || string(s.AuthID) != "auth/file.json" || u.Provider != "claude" || u.AuthIndex != "abcd" {
			t.Fatalf("secrets not passed: %+v", u)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(keep, []byte("sk-cpa")) || len(seen.Source) > 0 && bytes.Contains(seen.Source, []byte("example")) {
		t.Fatal("scratch buffer not zeroed")
	}
	enc, _ := json.Marshal(r)
	for _, s := range []string{key, src, "auth/file.json"} {
		if strings.Contains(string(enc), s) {
			t.Fatalf("decoded usage holds a secret field")
		}
	}
	if r.Latency.Seconds() != 1.5 || r.FailureStatus != 500 || r.FailureBody != "oops" || r.Tokens.InputTokens != 5 {
		t.Fatalf("usage = %+v", r)
	}
	// A long secret takes the heap path and is zeroed as well.
	long := strings.Repeat("k", 2000)
	_, err = DecodeUsage([]byte(`{"APIKey":"`+long+`"}`), func(_ *Usage, s UsageSecrets) { keep = s.APIKey })
	if err != nil || bytes.Contains(keep, []byte("kkkk")) {
		t.Fatalf("long scratch not zeroed (err %v)", err)
	}
}
