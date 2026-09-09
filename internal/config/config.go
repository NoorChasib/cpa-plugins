package config

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DatabasePath        string        `yaml:"database-path"`
	QueueCapacity       int           `yaml:"queue-capacity"`
	BatchSize           int           `yaml:"batch-size"`
	FlushInterval       time.Duration `yaml:"flush-interval"`
	RawRetention        time.Duration `yaml:"raw-retention"`
	MaintenanceInterval time.Duration `yaml:"maintenance-interval"`
	MaxDiskBytes        int64         `yaml:"max-disk-bytes"`
	MaxModels           int           `yaml:"max-models"`
	QueryTimeout        time.Duration `yaml:"query-timeout"`
	Enabled             bool          `yaml:"enabled"`
	Priority            int           `yaml:"priority"`
}

// Host-owned store metadata is decoded without expanding its contents and then
// discarded. It must never become part of runtime equality or path discovery.
// The complete YAML document, including this node, is bounded to 64 KiB.
type rawConfig struct {
	Config `yaml:",inline"`
	Store  yaml.Node `yaml:"store"`
}

func Parse(raw []byte) (Config, error) {
	decoded := &rawConfig{Config: Config{QueueCapacity: 8192, BatchSize: 256, FlushInterval: time.Second, RawRetention: 720 * time.Hour, MaintenanceInterval: time.Minute, MaxDiskBytes: 1 << 30, MaxModels: 10000, QueryTimeout: 5 * time.Second}}
	c := decoded.Config
	if len(raw) == 0 || len(raw) > 64<<10 {
		return c, errors.New("configuration is required and must be bounded")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return c, errors.New("invalid configuration")
	}
	if decoded.Store.Kind != 0 && decoded.Store.Kind != yaml.MappingNode {
		return c, errors.New("invalid configuration")
	}
	c = decoded.Config
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return c, errors.New("one configuration document required")
	}
	// An omitted path is resolved from the working directory captured by the
	// native instance, not from mutable process state while parsing YAML.
	if c.DatabasePath != "" && (len(c.DatabasePath) > 4096 || strings.ContainsRune(c.DatabasePath, 0) || !filepath.IsAbs(c.DatabasePath) || filepath.Clean(c.DatabasePath) != c.DatabasePath || filepath.Dir(c.DatabasePath) == c.DatabasePath) {
		return c, errors.New("clean absolute database-path required")
	}
	if c.QueueCapacity < 1 || c.QueueCapacity > 65536 || c.BatchSize < 1 || c.BatchSize > 4096 || c.BatchSize > c.QueueCapacity || c.FlushInterval < 10*time.Millisecond || c.FlushInterval > 10*time.Second || c.RawRetention < time.Hour || c.RawRetention > 8760*time.Hour || c.MaintenanceInterval < time.Second || c.MaintenanceInterval > time.Hour || c.MaxDiskBytes < 1<<20 || c.MaxDiskBytes > 1<<40 || c.MaxModels < 1 || c.MaxModels > 100000 || c.QueryTimeout < 100*time.Millisecond || c.QueryTimeout > 30*time.Second {
		return c, errors.New("configuration value outside supported bounds")
	}
	// These values belong to CPA; they do not restart the local collector.
	c.Enabled = false
	c.Priority = 0
	return c, nil
}
