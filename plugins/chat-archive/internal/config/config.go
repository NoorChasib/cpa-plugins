// Package config is the strict loader for plugins.configs.chat-archive, adapted from
// token-usage's internal/config: 64 KiB bound, KnownFields, one document, host-owned enabled and
// priority accepted and ignored, store kept as an opaque yaml.Node and discarded, and generic
// errors that never echo a value. Secrets are configured only by reference (*-env, *-file) and
// resolved by Resolve; they are never part of Config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// Capture modes.
const (
	CaptureFull     = "full"
	CaptureMetadata = "metadata"
	CaptureOff      = "off"
)

// Config is comparable: equality decides between an idempotent reconfigure and a restart.
// AccountLabels is held as a canonical string for that reason (see Labels).
type Config struct {
	ArchiveURL           string
	SourceTokenEnv       string
	SourceTokenFile      string
	HMACKeyID            string
	HMACKeyEnv           string
	HMACKeyFile          string
	ExactFingerprints    string // sorted, comma-joined 64-hex values
	AccountLabels        string // canonical encoding of the labels map
	DataDir              string // "" means the default under the captured working directory
	OutboxCapBytes       int64
	MinFreeBytes         int64
	MinFreePercent       int
	ReplayRetentionHours int
	MemoryLimitMiB       int
	Workers              int
	MaxRPCMiB            int
	Capture              string
	PushoverAppTokenEnv  string
	PushoverAppTokenFile string
	PushoverUserKeyEnv   string
	PushoverUserKeyFile  string
	AlertBacklogHours    int
}

type rawConfig struct {
	ArchiveURL           string            `yaml:"archive-url"`
	SourceTokenEnv       string            `yaml:"source-token-env"`
	SourceTokenFile      string            `yaml:"source-token-file"`
	HMACKeyID            string            `yaml:"hmac-key-id"`
	HMACKeyEnv           string            `yaml:"hmac-key-env"`
	HMACKeyFile          string            `yaml:"hmac-key-file"`
	ExactFingerprints    []string          `yaml:"exact-fingerprints"`
	AccountLabels        map[string]string `yaml:"account-labels"`
	DataDir              string            `yaml:"data-dir"`
	OutboxCapBytes       int64             `yaml:"outbox-cap-bytes"`
	MinFreeBytes         int64             `yaml:"min-free-bytes"`
	MinFreePercent       int               `yaml:"min-free-percent"`
	ReplayRetentionHours int               `yaml:"replay-retention-hours"`
	MemoryLimitMiB       int               `yaml:"memory-limit-mib"`
	Workers              int               `yaml:"workers"`
	MaxRPCMiB            int               `yaml:"max-rpc-mib"`
	Capture              string            `yaml:"capture"`
	PushoverAppTokenEnv  string            `yaml:"pushover-app-token-env"`
	PushoverAppTokenFile string            `yaml:"pushover-app-token-file"`
	PushoverUserKeyEnv   string            `yaml:"pushover-user-key-env"`
	PushoverUserKeyFile  string            `yaml:"pushover-user-key-file"`
	AlertBacklogHours    int               `yaml:"alert-backlog-hours"`
	Enabled              bool              `yaml:"enabled"`
	Priority             int               `yaml:"priority"`
	// Host-owned store metadata, decoded without expanding and discarded.
	Store yaml.Node `yaml:"store"`
}

// Default returns the defaults every key takes when the config omits it.
func Default() Config {
	return Config{
		HMACKeyID:            "k1",
		OutboxCapBytes:       21474836480,
		MinFreeBytes:         10737418240,
		MinFreePercent:       5,
		ReplayRetentionHours: 168,
		MemoryLimitMiB:       768,
		Workers:              2,
		MaxRPCMiB:            160,
		Capture:              CaptureFull,
		AlertBacklogHours:    6,
	}
}

var (
	errBounded  = errors.New("configuration is required and must be bounded")
	errInvalid  = errors.New("invalid configuration")
	envNameRE   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	keyIDRE     = regexp.MustCompile(`^[a-z0-9]{1,8}$`)
	hex64RE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tokenUsageD = string(filepath.Separator) + filepath.Join("plugins", "data", "token-usage")
)

func field(name string) error { return fmt.Errorf("invalid %s", name) }

// Parse decodes and validates the plugin config YAML. Errors name the key, never its value.
func Parse(raw []byte) (Config, error) {
	c := Default()
	if len(raw) == 0 || len(raw) > 64<<10 {
		return c, errBounded
	}
	d := &rawConfig{
		HMACKeyID: c.HMACKeyID, OutboxCapBytes: c.OutboxCapBytes, MinFreeBytes: c.MinFreeBytes,
		MinFreePercent: c.MinFreePercent, ReplayRetentionHours: c.ReplayRetentionHours,
		MemoryLimitMiB: c.MemoryLimitMiB, Workers: c.Workers, MaxRPCMiB: c.MaxRPCMiB,
		Capture: c.Capture, AlertBacklogHours: c.AlertBacklogHours,
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(d); err != nil {
		return c, errInvalid
	}
	if d.Store.Kind != 0 && d.Store.Kind != yaml.MappingNode {
		return c, errInvalid
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return c, errors.New("one configuration document required")
	}
	c = Config{
		ArchiveURL: d.ArchiveURL, SourceTokenEnv: d.SourceTokenEnv, SourceTokenFile: d.SourceTokenFile,
		HMACKeyID: d.HMACKeyID, HMACKeyEnv: d.HMACKeyEnv, HMACKeyFile: d.HMACKeyFile,
		DataDir: d.DataDir, OutboxCapBytes: d.OutboxCapBytes, MinFreeBytes: d.MinFreeBytes,
		MinFreePercent: d.MinFreePercent, ReplayRetentionHours: d.ReplayRetentionHours,
		MemoryLimitMiB: d.MemoryLimitMiB, Workers: d.Workers, MaxRPCMiB: d.MaxRPCMiB,
		Capture: d.Capture, PushoverAppTokenEnv: d.PushoverAppTokenEnv,
		PushoverAppTokenFile: d.PushoverAppTokenFile, PushoverUserKeyEnv: d.PushoverUserKeyEnv,
		PushoverUserKeyFile: d.PushoverUserKeyFile, AlertBacklogHours: d.AlertBacklogHours,
	}
	if c.ArchiveURL != "" {
		u, err := url.Parse(c.ArchiveURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.ContainsAny(c.ArchiveURL, "\x00\r\n") ||
			len(c.ArchiveURL) > 2048 {
			return c, field("archive-url")
		}
		if u.Path != "" && u.Path != "/" {
			return c, field("archive-url")
		}
		c.ArchiveURL = strings.TrimRight(c.ArchiveURL, "/")
	}
	for _, p := range []struct {
		name, env, file string
	}{
		{"source-token", c.SourceTokenEnv, c.SourceTokenFile},
		{"hmac-key", c.HMACKeyEnv, c.HMACKeyFile},
		{"pushover-app-token", c.PushoverAppTokenEnv, c.PushoverAppTokenFile},
		{"pushover-user-key", c.PushoverUserKeyEnv, c.PushoverUserKeyFile},
	} {
		if p.env != "" && p.file != "" {
			return c, field(p.name + "-env/-file (set one)")
		}
		if p.env != "" && !envNameRE.MatchString(p.env) {
			return c, field(p.name + "-env")
		}
		if p.file != "" && !cleanAbs(p.file) {
			return c, field(p.name + "-file")
		}
	}
	if !keyIDRE.MatchString(c.HMACKeyID) {
		return c, field("hmac-key-id")
	}
	fps := make([]string, 0, len(d.ExactFingerprints))
	for _, fp := range d.ExactFingerprints {
		if !hex64RE.MatchString(fp) {
			return c, field("exact-fingerprints")
		}
		fps = append(fps, fp)
	}
	sort.Strings(fps)
	c.ExactFingerprints = strings.Join(fps, ",")
	if len(d.AccountLabels) > 256 {
		return c, field("account-labels")
	}
	for k, v := range d.AccountLabels {
		if k == "" || len(k) > 128 || len([]rune(v)) > 128 || strings.ContainsAny(k+v, "\x00\n\r\t") {
			return c, field("account-labels")
		}
	}
	c.AccountLabels = encodeLabels(d.AccountLabels)
	if c.DataDir != "" {
		if !cleanAbs(c.DataDir) || isTokenUsageDir(c.DataDir) {
			return c, field("data-dir")
		}
	}
	switch {
	case c.OutboxCapBytes < 1<<30:
		return c, field("outbox-cap-bytes")
	case c.MinFreeBytes < 0:
		return c, field("min-free-bytes")
	case c.MinFreePercent < 0 || c.MinFreePercent > 50:
		return c, field("min-free-percent")
	case c.ReplayRetentionHours < 24 || c.ReplayRetentionHours > 720:
		return c, field("replay-retention-hours")
	case c.MemoryLimitMiB < 128 || c.MemoryLimitMiB > 4096:
		return c, field("memory-limit-mib")
	case c.Workers < 1 || c.Workers > 4:
		return c, field("workers")
	case c.MaxRPCMiB < 8 || c.MaxRPCMiB > 512:
		return c, field("max-rpc-mib")
	case c.Capture != CaptureFull && c.Capture != CaptureMetadata && c.Capture != CaptureOff:
		return c, field("capture")
	case c.AlertBacklogHours < 1 || c.AlertBacklogHours > 168:
		return c, field("alert-backlog-hours")
	}
	return c, nil
}

func cleanAbs(p string) bool {
	return len(p) <= 4096 && !strings.ContainsRune(p, 0) && filepath.IsAbs(p) &&
		filepath.Clean(p) == p && filepath.Dir(p) != p
}

func isTokenUsageDir(p string) bool {
	return strings.HasSuffix(p, tokenUsageD) || strings.Contains(p+string(filepath.Separator), tokenUsageD+string(filepath.Separator))
}

// encodeLabels gives a canonical, comparable form of the labels map.
func encodeLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// Labels decodes AccountLabels.
func (c Config) Labels() map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(c.AccountLabels, "\n") {
		k, v, ok := strings.Cut(line, "\x00")
		if ok {
			out[k] = v
		}
	}
	return out
}

// ExactFPs decodes ExactFingerprints.
func (c Config) ExactFPs() []string {
	if c.ExactFingerprints == "" {
		return nil
	}
	return strings.Split(c.ExactFingerprints, ",")
}

// Configured reports whether capture can run: an archive URL, a source token reference and an
// HMAC key reference; without them the plugin is unconfigured. The reason names the missing piece.
func (c Config) Configured() (bool, string) {
	switch {
	case c.ArchiveURL == "":
		return false, "archive_url_missing"
	case c.SourceTokenEnv == "" && c.SourceTokenFile == "":
		return false, "source_token_missing"
	case c.HMACKeyEnv == "" && c.HMACKeyFile == "":
		return false, "hmac_key_missing"
	}
	return true, ""
}

// Secrets are the resolved secret values. They are never logged, never part of status, and never
// stored outside memory.
type Secrets struct {
	SourceToken  string
	HMACKey      []byte
	PushoverApp  string
	PushoverUser string
}

// Resolve reads the referenced secrets. A file must be absolute, a regular file with mode 0600,
// a single link, not a symlink, owned by the current uid. Errors name the key only.
func (c Config) Resolve() (Secrets, error) {
	var s Secrets
	tok, err := readSecret(c.SourceTokenEnv, c.SourceTokenFile, "source-token")
	if err != nil {
		return s, err
	}
	key, err := readSecret(c.HMACKeyEnv, c.HMACKeyFile, "hmac-key")
	if err != nil {
		return s, err
	}
	if len(key) < 32 {
		return s, field("hmac-key (at least 32 bytes)")
	}
	if tok == "" {
		return s, field("source-token (empty)")
	}
	s.SourceToken, s.HMACKey = tok, []byte(key)
	if c.PushoverAppTokenEnv != "" || c.PushoverAppTokenFile != "" {
		if s.PushoverApp, err = readSecret(c.PushoverAppTokenEnv, c.PushoverAppTokenFile, "pushover-app-token"); err != nil {
			return s, err
		}
	}
	if c.PushoverUserKeyEnv != "" || c.PushoverUserKeyFile != "" {
		if s.PushoverUser, err = readSecret(c.PushoverUserKeyEnv, c.PushoverUserKeyFile, "pushover-user-key"); err != nil {
			return s, err
		}
	}
	return s, nil
}

func readSecret(env, file, name string) (string, error) {
	if env != "" {
		v := strings.TrimSpace(os.Getenv(env))
		if v == "" {
			return "", field(name + "-env (unset)")
		}
		return v, nil
	}
	if file == "" {
		return "", field(name + " (not configured)")
	}
	info, err := os.Lstat(file)
	if err != nil {
		return "", field(name + "-file (unreadable)")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || st.Uid != uint32(os.Geteuid()) ||
		st.Nlink != 1 || info.Size() > 64<<10 {
		return "", field(name + "-file (must be a 0600 regular file owned by this user)")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", field(name + "-file (unreadable)")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", field(name + "-file (unreadable)")
	}
	v := strings.TrimSpace(string(b))
	clear(b)
	if v == "" {
		return "", field(name + "-file (empty)")
	}
	return v, nil
}
