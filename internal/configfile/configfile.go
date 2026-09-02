// Package configfile locates CPA's config.yaml, reads the effective Claude
// and Codex header baselines out of it, and performs surgical in-place edits
// of the baseline keys with the yaml.v3 Node API so comments, key order, and
// unrelated settings are preserved. The rationale (why the file is the only
// lever, how CPA's watcher and its own WriteConfig behave, and why the write
// is in place rather than rename-based) is in docs/architecture.md sections
// 2.3 and 5.
package configfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/NoorChasib/cpa-plugin-auto-baseline/internal/fingerprint"
)

// YAML keys edited or read by the plugin (internal/config/config.go:131,138
// and config_types.go:115-131,147-150 in the audited CPA build).
const (
	keyClaudeHeaderDefaults = "claude-header-defaults"
	keyCodexHeaderDefaults  = "codex-header-defaults"
	keyCodex                = "codex"
	keyUserAgent            = "user-agent"
	keyPackageVersion       = "package-version"
	keyRuntimeVersion       = "runtime-version"
	keyDisableCodexCloaking = "disable-codex-cloaking"

	// BackupFileName is written into the backup directory before each write.
	BackupFileName = "config.yaml.auto-baseline.bak"

	// maxConfigBytes bounds how large a config file the plugin will touch.
	maxConfigBytes = 8 << 20
)

// Sentinel errors surfaced in status. Each carries a stable code prefix so
// operators can grep for it.
var (
	// ErrChanged is returned when the file changed between read and write.
	// The check narrows but cannot eliminate the race: other writers (CPA's
	// management console, an editor) do not participate in any lock.
	ErrChanged = errors.New("config_changed: config file changed during read-modify-write")
	// ErrMultiDocument is returned for a YAML stream with more than one
	// document; the plugin refuses to guess which one CPA reads.
	ErrMultiDocument = errors.New("multi_document_config: config file contains more than one YAML document")
	// ErrUnsupportedShape is returned when the target key exists but is not a
	// mapping (non-empty scalar, sequence, alias, or reached through an alias
	// or merge key). The plugin never destroys operator content.
	ErrUnsupportedShape = errors.New("unsupported_config_shape: baseline key is not a plain mapping")
	// ErrDuplicateKey is returned when a mapping the plugin traverses defines
	// the same key twice. CPA's own yaml.v3 struct decode rejects such a file
	// ("mapping key ... already defined"), so it could never be reloaded; the
	// plugin refuses to touch it rather than guess which value wins.
	ErrDuplicateKey = errors.New("duplicate_key: config file defines a mapping key more than once")
	// ErrCycle is returned when aliases or merge keys form a cycle. Following
	// it would overflow the stack and kill the host process.
	ErrCycle = errors.New("unsupported_config_shape: alias or merge-key cycle in config file")
)

// Traversal bounds for alias / merge-key resolution.
const (
	maxDerefDepth = 32
	// pluginID is this plugin's config key under plugins.configs.
	pluginID = "auto-baseline"
)

// Effective describes the baseline CPA currently applies for one provider.
type Effective struct {
	Provider       fingerprint.Provider
	Version        fingerprint.Version
	UserAgent      string
	PackageVersion string
	RuntimeVersion string
	// Explicit reports whether the user-agent key was present in the file;
	// when false the compiled default of the audited build is assumed.
	Explicit bool
	// Malformed is set when an explicit user-agent did not parse as a client
	// UA. CPA itself falls back to its compiled default version in that case
	// (helps/claude_device_profile.go:588-594), but the plugin must not treat
	// the compiled default as comparable: it refuses to promote for the
	// provider until the operator fixes the value.
	Malformed bool
	// Unsupported names a structural problem inside this provider's block
	// (duplicate key, alias/merge cycle) that makes the block unreadable and
	// uneditable; empty when the block is fine.
	Unsupported string
}

// Snapshot is what the plugin learned from one read of config.yaml.
type Snapshot struct {
	Path                 string
	SHA256               string
	Claude               Effective
	Codex                Effective
	DisableCodexCloaking bool
	// PluginsEnabled / InstanceEnabled mirror plugins.enabled and
	// plugins.configs.auto-baseline.enabled on disk. A missing key reads as
	// false, exactly like CPA (PluginInstanceConfig.Enabled nil -> false).
	// Both must be true for a write to proceed: the host disables a plugin on
	// reload without calling quiesce, so the file is the authority.
	PluginsEnabled  bool
	InstanceEnabled bool
}

// Blocked reports why the provider block cannot be compared against or
// edited, or "" when it can.
func (e Effective) Blocked() string {
	if e.Unsupported != "" {
		return e.Unsupported
	}
	if e.Malformed {
		return "baseline_malformed"
	}
	return ""
}

// ResolvePath decides which config.yaml to manage: an explicit override, the
// -config/--config flag of the running CPA process (Linux /proc, best effort),
// or <cwd>/config.yaml, which is CPA's own default
// (cmd/server/main.go:103, 518-527).
func ResolvePath(override string) (string, string) {
	if p := strings.TrimSpace(override); p != "" {
		return p, "config-path"
	}
	if p := configFlagFromCmdline(readCmdline()); p != "" {
		return p, "process -config flag"
	}
	wd, err := os.Getwd()
	if err != nil {
		return "config.yaml", "cwd default"
	}
	return filepath.Join(wd, "config.yaml"), "cwd default"
}

// readCmdline returns the NUL-separated argv of the current process on Linux
// and nil elsewhere.
func readCmdline() []byte {
	raw, err := os.ReadFile("/proc/self/cmdline")
	if err != nil {
		return nil
	}
	return raw
}

// cmdlineArgs splits a NUL-separated argv.
func cmdlineArgs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

// flagValue extracts the value of -name / --name from argv. It understands
// "-name value" and "-name=value".
func flagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		trimmed := strings.TrimLeft(arg, "-")
		if len(arg)-len(trimmed) == 0 || len(arg)-len(trimmed) > 2 {
			continue
		}
		if trimmed == name {
			if i+1 < len(args) && strings.TrimSpace(args[i+1]) != "" {
				return strings.TrimSpace(args[i+1]), true
			}
			return "", true
		}
		if value, ok := strings.CutPrefix(trimmed, name+"="); ok {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// configFlagFromCmdline extracts the value of -config / --config.
func configFlagFromCmdline(raw []byte) string {
	value, _ := flagValue(cmdlineArgs(raw), "config")
	return value
}

// DeploymentMode describes a CPA configuration source other than a plain
// local file. In these modes the file the plugin would edit is not the
// effective configuration, so automatic writes are disabled.
type DeploymentMode struct {
	// Name is "home", "postgres", "object-store", or "git-store"; empty when
	// CPA runs from a plain local file.
	Name string
	// Reason names the flag or environment variable that triggered detection.
	Reason string
}

// DetectDeploymentMode inspects argv and the environment for the flags and
// variables CPA reads in cmd/server/main.go (audited build): -home-jwt /
// HOME_JWT (home control plane), PGSTORE_DSN (postgres-backed config),
// OBJECTSTORE_ENDPOINT (object-store-backed config), GITSTORE_GIT_URL
// (git-backed config). CPA also accepts the lowercase spellings. Detection is
// best-effort and Linux-oriented (argv comes from /proc/self/cmdline).
func DetectDeploymentMode() DeploymentMode {
	return detectDeploymentMode(cmdlineArgs(readCmdline()), os.LookupEnv)
}

func detectDeploymentMode(args []string, lookup func(string) (string, bool)) DeploymentMode {
	envSet := func(names ...string) (string, bool) {
		for _, name := range names {
			if v, ok := lookup(name); ok && strings.TrimSpace(v) != "" {
				return name, true
			}
		}
		return "", false
	}
	if v, ok := flagValue(args, "home-jwt"); ok && v != "" {
		return DeploymentMode{Name: "home", Reason: "-home-jwt flag"}
	}
	if name, ok := envSet("HOME_JWT", "home_jwt"); ok {
		return DeploymentMode{Name: "home", Reason: name + " environment variable"}
	}
	if name, ok := envSet("PGSTORE_DSN", "pgstore_dsn"); ok {
		return DeploymentMode{Name: "postgres", Reason: name + " environment variable"}
	}
	if name, ok := envSet("OBJECTSTORE_ENDPOINT", "objectstore_endpoint"); ok {
		return DeploymentMode{Name: "object-store", Reason: name + " environment variable"}
	}
	if name, ok := envSet("GITSTORE_GIT_URL", "gitstore_git_url"); ok {
		return DeploymentMode{Name: "git-store", Reason: name + " environment variable"}
	}
	return DeploymentMode{}
}

// Read loads and parses the config file.
func Read(path string) (Snapshot, error) {
	raw, err := readBounded(path)
	if err != nil {
		return Snapshot{}, err
	}
	snap, _, err := parse(path, raw)
	return snap, err
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxConfigBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxConfigBytes)
	}
	return os.ReadFile(path)
}

func hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// parse decodes the document and extracts the baselines. It returns the
// root node so render can reuse it. Multi-document streams, duplicate
// top-level keys, and top-level alias/merge cycles are refused outright;
// problems confined to one provider's block are reported in that
// Effective.Unsupported so the other provider keeps working.
func parse(path string, raw []byte) (Snapshot, *yaml.Node, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
			return Snapshot{}, nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
		}
		var extra yaml.Node
		if err := dec.Decode(&extra); err == nil {
			return Snapshot{}, nil, ErrMultiDocument
		} else if !errors.Is(err, io.EOF) {
			return Snapshot{}, nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
		}
	}
	root, err := derefNode(documentRootRaw(&doc))
	if err != nil {
		return Snapshot{}, nil, err
	}
	if root != nil && root.Kind != yaml.MappingNode {
		return Snapshot{}, nil, fmt.Errorf("%s: top-level YAML is not a mapping", filepath.Base(path))
	}
	if err := checkDuplicateKeys(root); err != nil {
		return Snapshot{}, nil, err
	}
	snap := Snapshot{Path: path, SHA256: hash(raw)}

	claudeBlock, err := resolvedLookup(root, keyClaudeHeaderDefaults)
	if err != nil {
		return Snapshot{}, nil, err
	}
	snap.Claude = effectiveClaude(claudeBlock)

	codexBlock, err := resolvedLookup(root, keyCodexHeaderDefaults)
	if err != nil {
		return Snapshot{}, nil, err
	}
	snap.Codex = effectiveCodex(codexBlock)

	codexNode, err := resolvedLookup(root, keyCodex)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if b, ok := boolAt(codexNode, keyDisableCodexCloaking); ok {
		snap.DisableCodexCloaking = b
	}

	pluginsNode, err := resolvedLookup(root, "plugins")
	if err != nil {
		return Snapshot{}, nil, err
	}
	if b, ok := boolAt(pluginsNode, "enabled"); ok {
		snap.PluginsEnabled = b
	}
	configsNode, err := resolvedLookup(pluginsNode, "configs")
	if err != nil {
		return Snapshot{}, nil, err
	}
	instanceNode, err := resolvedLookup(configsNode, pluginID)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if b, ok := boolAt(instanceNode, "enabled"); ok {
		snap.InstanceEnabled = b
	}
	return snap, &doc, nil
}

// boolAt reads a boolean scalar at key, tolerating structural errors (they
// simply read as absent: the plugin only needs a positive "true").
func boolAt(mapping *yaml.Node, key string) (bool, bool) {
	v, err := resolvedLookup(mapping, key)
	if err != nil || v == nil || v.Kind != yaml.ScalarNode {
		return false, false
	}
	var b bool
	if err := v.Decode(&b); err != nil {
		return false, false
	}
	return b, true
}

// documentRootRaw returns the top-level node without following aliases.
func documentRootRaw(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	if doc.Kind == 0 {
		return nil
	}
	return doc
}

// derefNode follows alias nodes to their anchored target with a depth bound
// and a visited set, so an alias cycle yields ErrCycle instead of a stack
// overflow.
func derefNode(n *yaml.Node) (*yaml.Node, error) {
	seen := make(map[*yaml.Node]struct{}, 4)
	for depth := 0; n != nil && n.Kind == yaml.AliasNode; depth++ {
		if depth >= maxDerefDepth {
			return nil, ErrCycle
		}
		if _, dup := seen[n]; dup {
			return nil, ErrCycle
		}
		seen[n] = struct{}{}
		n = n.Alias
	}
	return n, nil
}

// isMergeKey reports whether a key node is an unquoted `<<` merge key.
// yaml.v3 tags it !!merge; a quoted "<<" is an ordinary string key.
func isMergeKey(key *yaml.Node) bool {
	return key != nil && key.Kind == yaml.ScalarNode && key.Tag == "!!merge"
}

// checkDuplicateKeys refuses a mapping that defines a scalar key twice
// (merge keys excluded: yaml.v3 allows repeated `<<`). CPA's decoder would
// reject the whole file, so it can never be hot-reloaded; refusing is the
// only safe answer.
func checkDuplicateKeys(mapping *yaml.Node) error {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	seen := make(map[string]struct{}, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k.Kind != yaml.ScalarNode || isMergeKey(k) {
			continue
		}
		if _, dup := seen[k.Value]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateKey, k.Value)
		}
		seen[k.Value] = struct{}{}
	}
	return nil
}

// lookupExplicit returns the value node of key in a mapping. Aliases are not
// followed and merge keys are not consulted. A duplicate key is an error.
func lookupExplicit(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	if err := checkDuplicateKeys(mapping); err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k.Kind == yaml.ScalarNode && !isMergeKey(k) && k.Value == key {
			return mapping.Content[i+1], nil
		}
	}
	return nil, nil
}

// resolvedLookup is the read-side lookup: it follows aliases and honours
// YAML merge keys (`<<: *base` or `<<: [*a, *b]`) so a config that shares a
// defaults anchor yields the values CPA actually decodes. Explicit keys win
// over merged ones; among merged sources the earlier alias wins, matching
// yaml.v3's decoder. Every mapping visited is checked for duplicate keys, and
// a visited set plus depth bound turn any alias/merge cycle into ErrCycle.
func resolvedLookup(mapping *yaml.Node, key string) (*yaml.Node, error) {
	return resolvedLookupGuarded(mapping, key, make(map[*yaml.Node]struct{}, 4), 0)
}

func resolvedLookupGuarded(mapping *yaml.Node, key string, visited map[*yaml.Node]struct{}, depth int) (*yaml.Node, error) {
	if depth >= maxDerefDepth {
		return nil, ErrCycle
	}
	mapping, err := derefNode(mapping)
	if err != nil {
		return nil, err
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	if _, dup := visited[mapping]; dup {
		return nil, ErrCycle
	}
	visited[mapping] = struct{}{}
	defer delete(visited, mapping)

	v, err := lookupExplicit(mapping, key)
	if err != nil {
		return nil, err
	}
	if v != nil {
		return derefNode(v)
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if !isMergeKey(mapping.Content[i]) {
			continue
		}
		merged, err := derefNode(mapping.Content[i+1])
		if err != nil {
			return nil, err
		}
		if merged == nil {
			continue
		}
		var sources []*yaml.Node
		if merged.Kind == yaml.SequenceNode {
			sources = merged.Content
		} else {
			sources = []*yaml.Node{merged}
		}
		for _, src := range sources {
			found, err := resolvedLookupGuarded(src, key, visited, depth+1)
			if err != nil {
				return nil, err
			}
			if found != nil {
				return found, nil
			}
		}
	}
	return nil, nil
}

func scalar(mapping *yaml.Node, key string) (string, bool, error) {
	v, err := resolvedLookup(mapping, key)
	if err != nil {
		return "", false, err
	}
	if v == nil || v.Kind != yaml.ScalarNode {
		return "", false, nil
	}
	return strings.TrimSpace(v.Value), true, nil
}

// unsupportedReason maps a structural error to the status/decision bucket.
func unsupportedReason(err error) string {
	if errors.Is(err, ErrDuplicateKey) {
		return "duplicate_key"
	}
	return "unsupported_config_shape"
}

func effectiveClaude(block *yaml.Node) Effective {
	eff := Effective{
		Provider:       fingerprint.ProviderClaude,
		Version:        fingerprint.CompiledClaudeBaselineVersion,
		UserAgent:      fingerprint.CompiledClaudeUserAgent,
		PackageVersion: fingerprint.CompiledClaudePackageVersion,
		RuntimeVersion: fingerprint.CompiledClaudeRuntimeVersion,
	}
	// CPA treats blank strings as absent (hdrDefault in
	// helps/claude_device_profile.go:133-139).
	ua, ok, err := scalar(block, keyUserAgent)
	if err != nil {
		eff.Unsupported = unsupportedReason(err)
		return eff
	}
	if ok && ua != "" {
		eff.Explicit = true
		eff.UserAgent = ua
		if v, okVersion := fingerprint.ParseClaudeUserAgentVersion(ua); okVersion {
			eff.Version = v
		} else {
			eff.Malformed = true
		}
	}
	if pv, ok, err := scalar(block, keyPackageVersion); err != nil {
		eff.Unsupported = unsupportedReason(err)
	} else if ok && pv != "" {
		eff.PackageVersion = pv
	}
	if rv, ok, err := scalar(block, keyRuntimeVersion); err != nil {
		eff.Unsupported = unsupportedReason(err)
	} else if ok && rv != "" {
		eff.RuntimeVersion = rv
	}
	return eff
}

func effectiveCodex(block *yaml.Node) Effective {
	eff := Effective{
		Provider:  fingerprint.ProviderCodex,
		Version:   fingerprint.CompiledCodexBaselineVersion,
		UserAgent: fingerprint.CompiledCodexUserAgent,
	}
	ua, ok, err := scalar(block, keyUserAgent)
	if err != nil {
		eff.Unsupported = unsupportedReason(err)
		return eff
	}
	if ok && ua != "" {
		eff.Explicit = true
		eff.UserAgent = ua
		if v, okVersion := fingerprint.ParseCodexUserAgentVersion(ua); okVersion {
			eff.Version = v
		} else {
			eff.Malformed = true
		}
	}
	return eff
}

// Apply performs one read-modify-write cycle:
//
//  1. read and hash the current file, parse it, and derive the effective
//     baseline;
//  2. call check(snapshot) so the caller can re-verify "strictly newer than
//     what is on disk right now" against fresh data;
//  3. mutate only the provider's baseline keys in the yaml.v3 node tree
//     (refusing unsupported shapes rather than destroying content);
//  4. copy the previous bytes to <backupDir>/config.yaml.auto-baseline.bak;
//  5. re-read and re-hash the file immediately before the destructive write
//     and fail with ErrChanged if another writer got there first;
//  6. write the new bytes in place.
//
// The returned Snapshot reflects the state read in step 1 (before the edit).
func Apply(path, backupDir string, candidate fingerprint.Candidate, check func(Snapshot) error) (Snapshot, error) {
	raw, err := readBounded(path)
	if err != nil {
		return Snapshot{}, err
	}
	snap, doc, err := parse(path, raw)
	if err != nil {
		return snap, err
	}
	if check != nil {
		if err := check(snap); err != nil {
			return snap, err
		}
	}
	updated, err := render(doc, raw, candidate)
	if err != nil {
		return snap, err
	}
	if bytes.Equal(updated, raw) {
		// Nothing to write; do not disturb the watcher.
		return snap, nil
	}
	if err := writeBackup(backupDir, raw); err != nil {
		return snap, err
	}
	// Detect concurrent external edits right before the destructive write.
	// The backup above may have taken time; re-read so the bytes we would
	// restore on failure are the freshest known-good content.
	current, err := readBounded(path)
	if err != nil {
		return snap, err
	}
	if hash(current) != snap.SHA256 {
		return snap, ErrChanged
	}
	if err := writeInPlace(path, updated, current); err != nil {
		return snap, err
	}
	return snap, nil
}

// render applies the edit to the parsed document and encodes it. raw is the
// original file bytes, used to preserve a trailing newline convention.
func render(doc *yaml.Node, raw []byte, candidate fingerprint.Candidate) ([]byte, error) {
	if doc == nil {
		doc = &yaml.Node{}
	}
	root, err := ensureRoot(doc)
	if err != nil {
		return nil, err
	}
	if err := checkDuplicateKeys(root); err != nil {
		return nil, err
	}
	switch candidate.Provider {
	case fingerprint.ProviderClaude:
		block, err := ensureMapping(root, keyClaudeHeaderDefaults)
		if err != nil {
			return nil, err
		}
		for _, kv := range [][2]string{
			{keyUserAgent, candidate.UserAgent},
			{keyPackageVersion, candidate.PackageVersion},
			{keyRuntimeVersion, candidate.RuntimeVersion},
		} {
			if err := setScalar(block, kv[0], kv[1]); err != nil {
				return nil, err
			}
		}
	case fingerprint.ProviderCodex:
		block, err := ensureMapping(root, keyCodexHeaderDefaults)
		if err != nil {
			return nil, err
		}
		if err := setScalar(block, keyUserAgent, candidate.UserAgent); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported provider %q", candidate.Provider)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("finish config encoding: %w", err)
	}
	out := buf.Bytes()
	if len(raw) > 0 && !bytes.HasSuffix(raw, []byte("\n")) {
		out = bytes.TrimRight(out, "\n")
	}
	return out, nil
}

// ensureRoot returns the top-level mapping, creating the document and mapping
// nodes for an empty file.
func ensureRoot(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode {
		doc.Kind = yaml.DocumentNode
		doc.Content = nil
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.AliasNode {
		return nil, ErrUnsupportedShape
	}
	if root.Kind != yaml.MappingNode {
		if !isEmptyScalar(root) {
			// parse() already rejects non-mapping roots; this only guards a
			// null scalar document such as "~" or "---".
			return nil, ErrUnsupportedShape
		}
		toMapping(root)
	}
	return root, nil
}

// isEmptyScalar reports whether n is a null/empty placeholder such as
// "key:" or "key: ~" or "key: null".
func isEmptyScalar(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.ScalarNode {
		return false
	}
	return n.Tag == "!!null" || (n.Style == 0 && strings.TrimSpace(n.Value) == "")
}

func toMapping(n *yaml.Node) {
	n.Kind = yaml.MappingNode
	n.Tag = "!!map"
	n.Value = ""
	n.Style = 0
	n.Content = nil
}

func hasMergeKey(mapping *yaml.Node) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if isMergeKey(mapping.Content[i]) {
			return true
		}
	}
	return false
}

// ensureMapping returns the mapping value for key, creating it when absent
// or converting an empty placeholder in place (so the key keeps its position
// and comments). Any other shape (non-empty scalar, sequence, alias, or a
// block that CPA would read through a merge key) is refused.
func ensureMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	v, err := lookupExplicit(parent, key)
	if err != nil {
		return nil, err
	}
	if v != nil {
		switch {
		case v.Kind == yaml.AliasNode:
			return nil, ErrUnsupportedShape
		case v.Kind == yaml.MappingNode:
			if hasMergeKey(v) {
				return nil, ErrUnsupportedShape
			}
			if err := checkDuplicateKeys(v); err != nil {
				return nil, err
			}
			return v, nil
		case isEmptyScalar(v):
			toMapping(v)
			return v, nil
		default:
			return nil, ErrUnsupportedShape
		}
	}
	merged, err := resolvedLookup(parent, key)
	if err != nil {
		return nil, err
	}
	if merged != nil {
		// Only reachable through a merge key: appending an explicit key would
		// silently shadow the operator's shared defaults.
		return nil, ErrUnsupportedShape
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valueNode)
	return valueNode, nil
}

// setScalar sets key to a plain string value, preserving an existing node's
// comments. Quoting is forced so version-like values such as 0.94.0 can never
// be re-read as floats. An alias value or a duplicate key is refused.
func setScalar(mapping *yaml.Node, key, value string) error {
	v, err := lookupExplicit(mapping, key)
	if err != nil {
		return err
	}
	if v != nil {
		if v.Kind == yaml.AliasNode {
			return ErrUnsupportedShape
		}
		if v.Kind != yaml.ScalarNode && !isEmptyScalar(v) {
			return ErrUnsupportedShape
		}
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Value = value
		v.Style = yaml.DoubleQuotedStyle
		v.Content = nil
		return nil
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle},
	)
	return nil
}

// BackupPath returns the backup file path inside dir.
func BackupPath(dir string) string { return filepath.Join(dir, BackupFileName) }

func writeBackup(dir string, previous []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	if err := os.WriteFile(BackupPath(dir), previous, 0o600); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	return nil
}

// configFile is the subset of *os.File writeInPlace needs; openConfigFile
// is the seam tests use to inject descriptor failures.
type configFile interface {
	io.Writer
	io.Seeker
	Truncate(int64) error
	Sync() error
	Close() error
}

var openConfigFile = func(path string) (configFile, error) {
	return os.OpenFile(path, os.O_RDWR, 0o644)
}

// writeInPlace rewrites the existing inode without ever truncating first:
// the new bytes are written from offset 0 on a single O_RDWR descriptor,
// then the file is truncated to the new length, fsynced, and closed. CPA's
// own WriteConfig (config_basic.go:101-118) truncates first; this order
// avoids the window where a crash or ENOSPC would leave an empty file for
// CPA's watcher to reload. It never renames, so a bind-mounted single file
// keeps working.
//
// If anything fails after the descriptor is open, the previous bytes are
// written back best-effort (on a fresh descriptor when the failure was
// Close itself) before the error is returned. After a successful close the
// file is re-read and its sha256 compared with the intended bytes; a
// mismatch is restored and reported. Crash atomicity still cannot be
// guaranteed on a single-file bind mount (no rename is possible); the backup
// in backup-dir exists for manual recovery.
func writeInPlace(path string, data, previous []byte) error {
	f, err := openConfigFile(path)
	if err != nil {
		return fmt.Errorf("open config for write: %w", err)
	}
	restoreOn := func(f configFile) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return
		}
		if _, err := f.Write(previous); err == nil {
			_ = f.Truncate(int64(len(previous)))
			_ = f.Sync()
		}
	}
	restore := func(cause error) error {
		restoreOn(f)
		_ = f.Close()
		return cause
	}
	restoreFresh := func(cause error) error {
		if g, err := openConfigFile(path); err == nil {
			restoreOn(g)
			_ = g.Close()
		}
		return cause
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return restore(fmt.Errorf("seek config: %w", err))
	}
	if n, err := f.Write(data); err != nil {
		return restore(fmt.Errorf("write config: %w", err))
	} else if n != len(data) {
		return restore(fmt.Errorf("write config: short write (%d of %d bytes)", n, len(data)))
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return restore(fmt.Errorf("truncate config: %w", err))
	}
	if err := f.Sync(); err != nil {
		return restore(fmt.Errorf("sync config: %w", err))
	}
	if err := f.Close(); err != nil {
		return restoreFresh(fmt.Errorf("close config: %w", err))
	}
	written, err := os.ReadFile(path)
	if err != nil {
		return restoreFresh(fmt.Errorf("verify config after write: %w", err))
	}
	if hash(written) != hash(data) {
		return restoreFresh(errors.New("verify config after write: on-disk bytes do not match the intended content"))
	}
	return nil
}

// Probe reports whether path exists as a regular file and whether it appears
// writable by this process. It never modifies the file.
func Probe(path string) (exists bool, writable bool, err error) {
	info, errStat := os.Stat(path)
	if errStat != nil {
		if errors.Is(errStat, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, errStat
	}
	if !info.Mode().IsRegular() {
		return true, false, fmt.Errorf("%s is not a regular file", path)
	}
	f, errOpen := os.OpenFile(path, os.O_WRONLY, 0)
	if errOpen != nil {
		return true, false, nil
	}
	_ = f.Close()
	return true, true, nil
}

// ProbeDir reports whether dir can receive the backup file: it is created
// if missing and a throwaway file is written and removed.
func ProbeDir(dir string) (writable bool, err error) {
	if strings.TrimSpace(dir) == "" {
		return false, errors.New("backup dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".auto-baseline-probe-*")
	if err != nil {
		return false, nil
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true, nil
}
