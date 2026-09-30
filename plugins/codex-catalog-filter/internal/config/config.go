// Package config parses plugins.configs.codex-catalog-filter from CPA.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
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
}

// Host-owned store metadata is decoded without expanding its contents and then
// discarded. The complete YAML document, including this node, is bounded.
type rawConfig struct {
	Config `yaml:",inline"`
	Store  yaml.Node `yaml:"store"`
}

// Parse validates the plugin's YAML and builds its rules. Unknown keys are
// rejected so a misspelled setting cannot silently change what Codex sees.
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
	enableNew := true
	switch strings.TrimSpace(c.NewModels) {
	case "", "enabled":
	case "disabled":
		enableNew = false
	default:
		return nil, errors.New("new-models must be enabled or disabled")
	}
	action := catalog.Remove
	switch strings.TrimSpace(c.Action) {
	case "", string(catalog.Remove):
	case string(catalog.Hide):
		action = catalog.Hide
	default:
		return nil, errors.New("action must be remove or hide")
	}
	if len(c.Models) > maxSwitches {
		return nil, fmt.Errorf("models lists more than %d switches", maxSwitches)
	}
	for slug := range c.Models {
		if slug == "" || len(slug) > maxSlugBytes || strings.TrimSpace(slug) != slug {
			return nil, fmt.Errorf("models keys must be non-empty model slugs, without surrounding spaces, of at most %d bytes", maxSlugBytes)
		}
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
	return &Settings{Rules: rules, CPAURL: origin, DataDir: c.DataDir}, nil
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
