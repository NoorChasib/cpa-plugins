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

func Parse(raw []byte) (Config, error) {
	c := Config{QueueCapacity: 8192, BatchSize: 256, FlushInterval: time.Second, RawRetention: 720 * time.Hour, MaintenanceInterval: time.Minute, MaxDiskBytes: 1 << 30, MaxModels: 10000, QueryTimeout: 5 * time.Second}
	if len(raw) == 0 || len(raw) > 64<<10 {
		return c, errors.New("configuration is required and must be bounded")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return c, errors.New("invalid configuration")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return c, errors.New("one configuration document required")
	}
	if c.DatabasePath == "" || len(c.DatabasePath) > 4096 || strings.ContainsRune(c.DatabasePath, 0) || !filepath.IsAbs(c.DatabasePath) || filepath.Clean(c.DatabasePath) != c.DatabasePath || filepath.Dir(c.DatabasePath) == c.DatabasePath {
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
