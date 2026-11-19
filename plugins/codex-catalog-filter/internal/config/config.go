// Package config parses plugins.configs.codex-catalog-filter from CPA.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"gopkg.in/yaml.v3"
)

const (
	maxDocumentBytes = 256 << 10
	maxSwitches      = 4096
	maxSlugBytes     = 256
	// DefaultCPAURL is CPA's listener as seen from inside its own process in
	// the standard image. The plugin fetches the unfiltered catalog from it.
	DefaultCPAURL = "http://127.0.0.1:8317"

	// AntigravityKey and ClaudeCodeKey are the per-client top-level keys. Each
	// is its own key so the page can PATCH one client without rewriting the
	// others: CPA's PATCH /v0/management/plugins/:id/config replaces whole
	// top-level keys.
	AntigravityKey = "antigravity"
	ClaudeCodeKey  = "claude-code"
)

type Config struct {
	// Models holds the per-model switches the settings page writes.
	Models    map[string]bool `yaml:"models"`
	NewModels string          `yaml:"new-models"`
	Action    string          `yaml:"action"`
	CPAURL    string          `yaml:"cpa-url"`
	DataDir   string          `yaml:"data-dir"`
	Enabled   bool            `yaml:"enabled"`
	Priority  int             `yaml:"priority"`
}

// Settings is the validated, immutable result of Parse.
type Settings struct {
	Rules *catalog.Rules
	// CPAURL is an origin without a trailing slash, such as http://127.0.0.1:8317.
	CPAURL string
	// DataDir is a clean absolute path, or empty for the plugin's default.
	DataDir string
	// Antigravity and ClaudeCode are validated independently of the rest: an
	// invalid subtree is reported in Err and never fails Parse, so a typo in
	// one client cannot take down the Codex route.
	Antigravity Antigravity
	ClaudeCode  ClaudeCode
}

// Source says where a client's effective settings come from.
type Source string

const (
	// FromConfig: the client's subtree is valid (or absent) and applied.
	FromConfig Source = "config"
	// FromLastGood: the subtree is invalid; the last valid settings this
	// process applied are kept.
	FromLastGood Source = "last-good"
	// FromDefaults: the subtree is invalid and nothing valid was applied
	// before; the client's defaults apply.
	FromDefaults Source = "defaults"
)

// Antigravity is the antigravity subtree: the list served at the agy route.
type Antigravity struct {
	// Rules are the per-model switches; models without one follow new-models,
	// which defaults to disabled.
	Rules        *catalog.Rules
	DefaultModel string
	TitleModel   string
	// Configured reports whether the subtree is present in the configuration.
	Configured bool
	// Err is the validation error of the subtree, empty when it is valid. When
	// set, the other fields hold the effective settings named by Source.
	Err    string
	Source Source
}

// ClaudeCode is the claude-code subtree: the in-place rewrite of CPA's
// Claude-format model list for Claude Code.
type ClaudeCode struct {
	// Filter declares the response interceptor and rewrites Claude Code's
	// model list. It defaults to true.
	Filter bool
	// Rules are keyed by real (de-cloaked) model ids; models without a switch
	// follow new-models, which defaults to enabled.
	Rules      *catalog.Rules
	Configured bool
	Err        string
	Source     Source
}

// DefaultAntigravity is the antigravity client with no subtree configured.
func DefaultAntigravity() Antigravity {
	rules, _ := catalog.NewRules(nil, false, catalog.Remove)
	return Antigravity{Rules: rules, Source: FromConfig}
}

// DefaultClaudeCode is the claude-code client with no subtree configured.
func DefaultClaudeCode() ClaudeCode {
	rules, _ := catalog.NewRules(nil, true, catalog.Remove)
	return ClaudeCode{Filter: true, Rules: rules, Source: FromConfig}
}

// Host-owned store metadata is decoded without expanding its contents and then
// discarded. The complete YAML document, including this node, is bounded.
// Client subtrees are decoded as nodes and validated on their own.
type rawConfig struct {
	Config      `yaml:",inline"`
	Store       yaml.Node `yaml:"store"`
	Antigravity yaml.Node `yaml:"antigravity"`
	ClaudeCode  yaml.Node `yaml:"claude-code"`
}

// Parse validates the plugin's YAML and builds its rules. Unknown top-level
// keys are rejected so a misspelled setting cannot silently change what Codex
// sees. A client subtree that fails validation is reported in its Err field
// with the client's defaults (FromDefaults); the caller may substitute the
// last valid settings.
func Parse(raw []byte) (*Settings, error) {
	if len(raw) == 0 || len(raw) > maxDocumentBytes {
		return nil, errors.New("configuration is required and must be bounded")
	}
	decoded := &rawConfig{}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return nil, errors.New("invalid configuration")
	}
	if decoded.Store.Kind != 0 && decoded.Store.Kind != yaml.MappingNode {
		return nil, errors.New("invalid configuration")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("one configuration document required")
	}
	c := decoded.Config
	enableNew, err := parseNewModels(c.NewModels, true)
	if err != nil {
		return nil, err
	}
	action := catalog.Remove
	switch strings.TrimSpace(c.Action) {
	case "", string(catalog.Remove):
	case string(catalog.Hide):
		action = catalog.Hide
	default:
		return nil, errors.New("action must be remove or hide")
	}
	if err := checkSwitches(c.Models); err != nil {
		return nil, err
	}
	rules, err := catalog.NewRules(c.Models, enableNew, action)
	if err != nil {
		return nil, err
	}
	origin, err := parseOrigin(c.CPAURL)
	if err != nil {
		return nil, err
	}
	if c.DataDir != "" && (len(c.DataDir) > 4096 || strings.ContainsRune(c.DataDir, 0) || !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) != c.DataDir || filepath.Dir(c.DataDir) == c.DataDir) {
		return nil, errors.New("data-dir must be a clean absolute path")
	}
	return &Settings{
		Rules:       rules,
		CPAURL:      origin,
		DataDir:     c.DataDir,
		Antigravity: parseAntigravity(&decoded.Antigravity),
		ClaudeCode:  parseClaudeCode(&decoded.ClaudeCode),
	}, nil
}

func parseNewModels(value string, fallback bool) (bool, error) {
	switch strings.TrimSpace(value) {
	case "":
		return fallback, nil
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	default:
		return false, errors.New("new-models must be enabled or disabled")
	}
}

func checkSwitches(models map[string]bool) error {
	if len(models) > maxSwitches {
		return fmt.Errorf("models lists more than %d switches", maxSwitches)
	}
	for slug := range models {
		if !validModelID(slug) {
			return fmt.Errorf("models keys must be non-empty model ids, without surrounding spaces, of at most %d bytes", maxSlugBytes)
		}
	}
	return nil
}

func validModelID(id string) bool {
	return id != "" && len(id) <= maxSlugBytes && strings.TrimSpace(id) == id
}

type antigravityConfig struct {
	Models       map[string]bool `yaml:"models"`
	NewModels    string          `yaml:"new-models"`
	DefaultModel string          `yaml:"default-model"`
	TitleModel   string          `yaml:"title-model"`
}

type claudeCodeConfig struct {
	Filter    *bool           `yaml:"filter"`
	Models    map[string]bool `yaml:"models"`
	NewModels string          `yaml:"new-models"`
}

// fieldKind is the YAML a client subtree key must hold. The check uses the
// node's resolved tag, so YAML 1.1 words such as yes, no, on, and off (which
// yaml.v3 would still decode into a bool) and numbers given as model ids are
// rejected. The Codex top-level models map keeps accepting them, as in 0.1.1.
type fieldKind int

const (
	boolField     fieldKind = iota // true or false
	stringField                    // a string
	switchesField                  // a mapping of string ids to true or false
)

var (
	antigravityFields = map[string]fieldKind{"models": switchesField, "new-models": stringField, "default-model": stringField, "title-model": stringField}
	claudeCodeFields  = map[string]fieldKind{"filter": boolField, "models": switchesField, "new-models": stringField}
)

func parseAntigravity(node *yaml.Node) Antigravity {
	out := DefaultAntigravity()
	present, err := checkSubtree(node, AntigravityKey, antigravityFields)
	out.Configured = present
	if err != nil {
		out.Err, out.Source = err.Error(), FromDefaults
		return out
	}
	if !present {
		return out
	}
	var c antigravityConfig
	if node.Decode(&c) != nil {
		out.Err, out.Source = AntigravityKey+": invalid value; switches must be true or false", FromDefaults
		return out
	}
	enableNew, err := parseNewModels(c.NewModels, false)
	if err != nil {
		out.Err, out.Source = AntigravityKey+": "+err.Error(), FromDefaults
		return out
	}
	if err := checkSwitches(c.Models); err != nil {
		out.Err, out.Source = AntigravityKey+": "+err.Error(), FromDefaults
		return out
	}
	for name, value := range map[string]string{"default-model": c.DefaultModel, "title-model": c.TitleModel} {
		if value != "" && !validModelID(value) {
			out.Err, out.Source = fmt.Sprintf("%s: %s must be a model id without surrounding spaces, of at most %d bytes", AntigravityKey, name, maxSlugBytes), FromDefaults
			return out
		}
	}
	rules, err := catalog.NewRules(c.Models, enableNew, catalog.Remove)
	if err != nil {
		out.Err, out.Source = AntigravityKey+": invalid switches", FromDefaults
		return out
	}
	out.Rules, out.DefaultModel, out.TitleModel = rules, c.DefaultModel, c.TitleModel
	return out
}

func parseClaudeCode(node *yaml.Node) ClaudeCode {
	out := DefaultClaudeCode()
	present, err := checkSubtree(node, ClaudeCodeKey, claudeCodeFields)
	out.Configured = present
	if err != nil {
		out.Err, out.Source = err.Error(), FromDefaults
		return out
	}
	if !present {
		return out
	}
	var c claudeCodeConfig
	if node.Decode(&c) != nil {
		out.Err, out.Source = ClaudeCodeKey+": invalid value; filter and switches must be true or false", FromDefaults
		return out
	}
	enableNew, err := parseNewModels(c.NewModels, true)
	if err != nil {
		out.Err, out.Source = ClaudeCodeKey+": "+err.Error(), FromDefaults
		return out
	}
	if err := checkSwitches(c.Models); err != nil {
		out.Err, out.Source = ClaudeCodeKey+": "+err.Error(), FromDefaults
		return out
	}
	rules, err := catalog.NewRules(c.Models, enableNew, catalog.Remove)
	if err != nil {
		out.Err, out.Source = ClaudeCodeKey+": invalid switches", FromDefaults
		return out
	}
	out.Rules = rules
	if c.Filter != nil {
		out.Filter = *c.Filter
	}
	return out
}

// checkSubtree reports whether a client subtree is present and rejects a
// non-mapping value, unknown or duplicate keys, and values of the wrong YAML
// type (fieldKind). A null value counts as absent, as after the page sends
// null to delete the key, and so does a null field.
func checkSubtree(node *yaml.Node, name string, fields map[string]fieldKind) (bool, error) {
	switch {
	case node == nil || node.Kind == 0:
		return false, nil
	case node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null":
		return false, nil
	case node.Kind != yaml.MappingNode:
		return true, fmt.Errorf("%s must be a mapping", name)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return true, fmt.Errorf("%s: keys must be names such as models", name)
		}
		kind, known := fields[key.Value]
		if !known {
			return true, fmt.Errorf("%s: unknown key %s", name, quoted(key.Value))
		}
		if seen[key.Value] {
			return true, fmt.Errorf("%s: duplicate key %s", name, quoted(key.Value))
		}
		seen[key.Value] = true
		if err := checkField(name, key.Value, kind, value); err != nil {
			return true, err
		}
	}
	return true, nil
}

func checkField(name, key string, kind fieldKind, value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && value.ShortTag() == "!!null" {
		return nil
	}
	switch kind {
	case boolField:
		if !isBool(value) {
			return fmt.Errorf("%s: %s must be true or false", name, key)
		}
	case stringField:
		if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!str" {
			return fmt.Errorf("%s: %s must be a string", name, key)
		}
	case switchesField:
		if value.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: %s must map model ids to true or false", name, key)
		}
		for i := 0; i+1 < len(value.Content); i += 2 {
			id, on := value.Content[i], value.Content[i+1]
			if id.Kind != yaml.ScalarNode || id.ShortTag() != "!!str" {
				return fmt.Errorf("%s: %s keys must be model ids written as strings", name, key)
			}
			if !isBool(on) {
				return fmt.Errorf("%s: %s: the switch for %s must be true or false", name, key, quoted(id.Value))
			}
		}
	}
	return nil
}

func isBool(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.ShortTag() == "!!bool"
}

// quoted bounds and quotes a value from the configuration for an error
// message, which the page and the state route show.
func quoted(value string) string {
	const limit = 64
	if len(value) > limit {
		return strconv.Quote(value[:limit]) + "..."
	}
	return strconv.Quote(value)
}

// parseOrigin accepts only scheme://host[:port], so the configured value can
// never smuggle a path, query, or credentials into the catalog request.
func parseOrigin(raw string) (string, error) {
	if raw == "" {
		return DefaultCPAURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("cpa-url must be an http or https origin such as http://127.0.0.1:8317")
	}
	return u.Scheme + "://" + u.Host, nil
}
