// Package plugin answers CPA's native calls for Codex Catalog Filter.
package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/config"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/protocol"
)

const ID = "codex-catalog-filter"

var Version = "0.1.0"

// sourceFormat is the handler type CPA reports for /v1/models, including the
// Codex catalog it serves when the request carries client_version.
const sourceFormat = "openai"

type Plugin struct {
	lifecycle sync.Mutex
	rules     atomic.Pointer[catalog.Rules]
	terminal  atomic.Bool
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		// Nothing runs in the background, so there is nothing to drain.
		return struct{}{}, nil
	case protocol.MethodPluginShutdown:
		p.Shutdown()
		return struct{}{}, nil
	case protocol.MethodResponseInterceptAfter:
		return p.Intercept(raw), nil
	default:
		return nil, errors.New("unknown method")
	}
}

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var request protocol.LifecycleRequest
	if json.Unmarshal(raw, &request) != nil || request.SchemaVersion < protocol.SchemaVersion {
		return protocol.Registration{}, errors.New("schema 6 lifecycle request required")
	}
	// Parsing inside the lock keeps concurrent reconfigurations in call order.
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	rules, err := config.Parse(request.ConfigYAML)
	if err != nil {
		return protocol.Registration{}, err
	}
	if p.terminal.Load() {
		return protocol.Registration{}, errors.New("plugin is shut down")
	}
	// A rejected reconfiguration returns above and keeps the previous rules.
	p.rules.Store(rules)
	return registration(), nil
}

func (p *Plugin) Shutdown() {
	p.lifecycle.Lock()
	defer p.lifecycle.Unlock()
	p.terminal.Store(true)
	p.rules.Store(nil)
}

// Screen reports whether a response.intercept_after request could carry a
// model list. Model lists are the only responses CPA sends with an empty model
// and no request body, and CPA encodes SourceFormat and Model before any
// header or body. Screen stops at the first disqualifying field, so an
// ordinary response is rejected after about a hundred bytes, whatever its size.
// raw may be host memory and is never retained. A true result is only a
// candidate; Intercept makes the decision.
func Screen(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false
		}
		switch key {
		case "SourceFormat":
			var v string
			if dec.Decode(&v) != nil || v != sourceFormat {
				return false
			}
		case "Model", "RequestedModel":
			var v string
			if dec.Decode(&v) != nil || v != "" {
				return false
			}
		case "Stream":
			var v bool
			if dec.Decode(&v) != nil || v {
				return false
			}
		case "OriginalRequest", "RequestBody":
			var v presence
			if dec.Decode(&v) != nil || bool(v) {
				return false
			}
		case "Body":
			return true
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return false
			}
		}
	}
	return true
}

// Intercept rewrites a Codex model catalog and leaves every other response
// alone. It never fails: an empty result tells CPA to keep what it had.
//
// CPA calls this for every non-streaming response, so the checks run cheapest
// first: Screen, then a decode that copies no body (only a short prefix is
// decoded and the request bodies are only tested for presence), and only then
// the catalog itself.
func (p *Plugin) Intercept(raw []byte) protocol.ResponseInterceptResponse {
	var unchanged protocol.ResponseInterceptResponse
	rules := p.rules.Load()
	if rules.Idle() || !Screen(raw) {
		return unchanged
	}
	var head struct {
		SourceFormat    string
		Model           string
		RequestedModel  string
		Stream          bool
		StatusCode      int
		OriginalRequest presence
		RequestBody     presence
		Body            bodyPrefix
	}
	if json.Unmarshal(raw, &head) != nil ||
		head.SourceFormat != sourceFormat || head.Model != "" || head.RequestedModel != "" || head.Stream ||
		(head.StatusCode != 0 && head.StatusCode != 200) ||
		bool(head.OriginalRequest) || bool(head.RequestBody) || !looksLikeCatalog(head.Body) {
		return unchanged
	}
	var full protocol.ResponseInterceptRequest
	if json.Unmarshal(raw, &full) != nil {
		return unchanged
	}
	body := rules.Rewrite(full.Body)
	if body == nil {
		return unchanged
	}
	// CPA v8.0.4 sets neither header on model lists. Should a later version
	// derive them from the unfiltered bytes, they would describe a different
	// body, so they are dropped rather than passed on stale.
	var clear []string
	for key := range full.ResponseHeaders {
		if strings.EqualFold(key, "ETag") || strings.EqualFold(key, "Content-Length") {
			clear = append(clear, key)
		}
	}
	return protocol.ResponseInterceptResponse{Body: body, ClearHeaders: clear}
}

// presence records whether a JSON value is a non-empty string, without keeping it.
// CPA encodes a nil []byte as null and an empty one as "".
type presence bool

func (p *presence) UnmarshalJSON(raw []byte) error {
	*p = presence(!bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`)))
	return nil
}

// sniffChars is the base64 prefix decoded for the catalog check: 48 bytes.
const sniffChars = 64

// bodyPrefix decodes only the start of a base64 JSON string.
type bodyPrefix []byte

func (b *bodyPrefix) UnmarshalJSON(raw []byte) error {
	*b = nil
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil
	}
	encoded := raw[1 : len(raw)-1]
	encoded = encoded[:min(len(encoded), sniffChars)&^3]
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, err := base64.StdEncoding.Decode(decoded, encoded)
	if err == nil {
		*b = decoded[:n]
	}
	return nil
}

// looksLikeCatalog reports whether a body starts `{"models":`, which is how
// CPA serializes the single-key Codex catalog. OpenAI and Grok lists start
// with "object" or "data"; Gemini's also starts with "models" but is sent with
// SourceFormat "gemini", and Rewrite rejects its entries anyway.
func looksLikeCatalog(prefix []byte) bool {
	s := bytes.TrimLeft(prefix, " \t\r\n")
	if len(s) == 0 || s[0] != '{' {
		return false
	}
	return bytes.HasPrefix(bytes.TrimLeft(s[1:], " \t\r\n"), []byte(`"models"`))
}

func registration() protocol.Registration {
	return protocol.Registration{SchemaVersion: protocol.SchemaVersion, Metadata: protocol.Metadata{Name: "Codex Catalog Filter", Version: Version, Author: "NoorChasib", GitHubRepository: "https://github.com/NoorChasib/cpa-plugins", ConfigFields: []protocol.ConfigField{
		{Name: "include", Type: "array", Description: "Glob patterns for the model slugs Codex should keep, such as gpt-[0-9]* and codex-*. When empty, every catalog passes through unchanged."},
		{Name: "exclude", Type: "array", Description: "Glob patterns that drop a slug even when include matches it."},
		{Name: "action", Type: "enum", EnumValues: []string{string(catalog.Remove), string(catalog.Hide)}, Description: "remove (default) deletes other entries from the catalog; hide keeps them with visibility hide so they stay selectable by name."},
	}}, Capabilities: protocol.RegistrationCapabilities{ResponseInterceptor: true}}
}
