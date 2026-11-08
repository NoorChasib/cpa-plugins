package canon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

func addCanonSeeds(f *testing.F) {
	ins, _ := filepath.Glob(filepath.Join("testdata", "canon", "*.in"))
	for _, in := range ins {
		if b, err := os.ReadFile(in); err == nil {
			f.Add(b)
		}
	}
	for _, s := range []string{`{}`, `[]`, `""`, `0`, `{"a":[{"b":"é\ud800"}],"a":1}`, `[1e400,-0.0,1E-7]`, `{"\u0000":"\/"}`} {
		f.Add([]byte(s))
	}
}

// FuzzCanonicalize: canon/v1 accepts exactly what encoding/json accepts, its output is a fixed point
// with no flags, and it means what encoding/json reads from the input (with the same last-wins and
// U+FFFD rules, numbers compared as literals).
func FuzzCanonicalize(f *testing.F) {
	addCanonSeeds(f)
	f.Fuzz(func(t *testing.T, in []byte) {
		out, flags, err := Canonicalize(in)
		if valid := json.Valid(in); valid != (err == nil) {
			t.Fatalf("json.Valid=%v but Canonicalize err=%v for %q", valid, err, in)
		}
		if err != nil {
			return
		}
		again, flags2, err := Canonicalize(out)
		if err != nil || !bytes.Equal(again, out) || len(flags2) != 0 {
			t.Fatalf("not a fixed point: %q -> %q (%v %v)", out, again, flags2, err)
		}
		if slices.Contains(flags, wire.FlagBadUTF8) {
			return // encoding/json's replacement of invalid bytes may group them differently
		}
		var a, b any
		da := json.NewDecoder(bytes.NewReader(in))
		da.UseNumber()
		db := json.NewDecoder(bytes.NewReader(out))
		db.UseNumber()
		if da.Decode(&a) != nil || db.Decode(&b) != nil {
			t.Fatalf("encoding/json rejects %q or %q", in, out)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("meaning changed: %q -> %q", in, out)
		}
	})
}

// FuzzSplit: any body in any format splits without panicking, and every split satisfies the piece,
// stub and reference invariants.
func FuzzSplit(f *testing.F) {
	ins, _ := filepath.Glob(filepath.Join("testdata", "split", "*.in.json"))
	e := newEnv(f)
	for i, in := range ins {
		if b, err := os.ReadFile(in); err == nil {
			f.Add(e.expand(b), uint8(i), i%2 == 0)
		}
	}
	formats := []string{FormatClaude, FormatOpenAIChat, FormatOpenAIResponses, FormatGemini, FormatInteractions,
		FormatMediaPrefix + "images", FormatMultipart, FormatOpaque, "unknown"}
	for i, s := range []string{
		`{"system":"s","messages":[{"role":"user","content":[{"type":"text","text":"t"},{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","data":"QQ=="}}]}]}]}`,
		`{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","function":{"arguments":"{\"a\":1}"}}],"reasoning_details":[{"type":"reasoning.encrypted","data":"x"}]}]}`,
		`{"instructions":"i","input":[{"type":"function_call","arguments":"[1]"},{"role":"user","content":"x"},{"type":"reasoning","encrypted_content":"e"}],"output":[]}`,
		`{"systemInstruction":{"parts":[{"text":"s"}]},"contents":[{"parts":[{"functionCall":{}},{"text":"x","thought":true}]}],"candidates":[{"content":{"parts":[]}}]}`,
		`{"input":[{"type":"thought","signature":"s"}],"system_instruction":{"text":"x"}}`,
		`{"data":[{"b64_json":"QUFB","url":"https://h.test/p?q=1"}]}`,
	} {
		f.Add([]byte(s), uint8(i), i%2 == 1)
	}
	f.Fuzz(func(t *testing.T, body []byte, sel uint8, response bool) {
		e := newEnv(t)
		format := formats[int(sel)%len(formats)]
		in := Input{Format: format, Body: body}
		if sel&0x80 != 0 {
			in.ContentType = "multipart/form-data; boundary=synthboundary"
		}
		s := newSplitter(t.Context(), e.m, fakeStripURL, Options{MaxAttachment: 4096})
		sp, err := s.split(in, response)
		if err != nil {
			t.Fatalf("split: %v", err)
		}
		checkInvariants(t, sp)
		if format == FormatClaude || format == FormatOpenAIResponses {
			if ms, err := e.splitter(Options{}).messages(format, body); err == nil {
				checkInvariants(t, ms)
			}
		}
	})
}
