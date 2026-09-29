// Package config parses plugins.configs.codex-catalog-filter from CPA.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"gopkg.in/yaml.v3"
)

const (
	maxDocumentBytes = 64 << 10
	maxPatterns      = 256
	maxPatternBytes  = 256
	// DefaultCPAURL is CPA's listener as seen from inside its own process in
	// the standard image. The plugin fetches the unfiltered catalog from it.
	DefaultCPAURL = "http://127.0.0.1:8317"
)

type Config struct {
	Include  []string `yaml:"include"`
	Exclude  []string `yaml:"exclude"`
	Action   string   `yaml:"action"`
	CPAURL   string   `yaml:"cpa-url"`
	Enabled  bool     `yaml:"enabled"`
	Priority int      `yaml:"priority"`
}

// Settings is the validated, immutable result of Parse.
type Settings struct {
	Rules *catalog.Rules
	// CPAURL is an origin without a trailing slash, such as http://127.0.0.1:8317.
	CPAURL string
}

// Host-owned store metadata is decoded without expanding its contents and then
// discarded. The complete YAML document, including this node, is bounded.
type rawConfig struct {
	Config `yaml:",inline"`
	Store  yaml.Node `yaml:"store"`
}

// Parse validates the plugin's YAML and compiles its rules. Unknown keys are
// rejected so a misspelled include cannot silently disable filtering.
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
	action := catalog.Remove
	switch strings.TrimSpace(c.Action) {
	case "", string(catalog.Remove):
	case string(catalog.Hide):
		action = catalog.Hide
	default:
		return nil, errors.New("action must be remove or hide")
	}
	for name, patterns := range map[string][]string{"include": c.Include, "exclude": c.Exclude} {
		if len(patterns) > maxPatterns {
			return nil, fmt.Errorf("%s lists more than %d patterns", name, maxPatterns)
		}
		for _, pattern := range patterns {
			if pattern == "" || len(pattern) > maxPatternBytes || strings.TrimSpace(pattern) != pattern {
				return nil, fmt.Errorf("%s patterns must be non-empty, without surrounding spaces, and at most %d bytes", name, maxPatternBytes)
			}
		}
	}
	rules, err := catalog.NewRules(c.Include, c.Exclude, action)
	if err != nil {
		return nil, errors.New("invalid include or exclude pattern")
	}
	origin, err := parseOrigin(c.CPAURL)
	if err != nil {
		return nil, err
	}
	return &Settings{Rules: rules, CPAURL: origin}, nil
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
