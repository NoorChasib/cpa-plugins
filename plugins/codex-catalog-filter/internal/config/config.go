// Package config parses plugins.configs.codex-catalog-filter from CPA.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/catalog"
	"gopkg.in/yaml.v3"
)

const (
	maxDocumentBytes = 64 << 10
	maxPatterns      = 256
	maxPatternBytes  = 256
)

type Config struct {
	Include  []string `yaml:"include"`
	Exclude  []string `yaml:"exclude"`
	Action   string   `yaml:"action"`
	Enabled  bool     `yaml:"enabled"`
	Priority int      `yaml:"priority"`
}

// Host-owned store metadata is decoded without expanding its contents and then
// discarded. The complete YAML document, including this node, is bounded.
type rawConfig struct {
	Config `yaml:",inline"`
	Store  yaml.Node `yaml:"store"`
}

// Parse validates the plugin's YAML and compiles its rules. Unknown keys are
// rejected so a misspelled include cannot silently disable filtering.
func Parse(raw []byte) (*catalog.Rules, error) {
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
	return rules, nil
}
