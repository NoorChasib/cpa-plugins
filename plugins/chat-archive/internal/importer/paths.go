package importer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root names: the first segment of every file key (Locator.FileKey).
const (
	RootClaudeProjects = "claude-projects"
	RootCodexSessions  = "codex-sessions"
	RootClaudeSkills   = "claude-skills"
)

// Paths are the importer's files under one home directory.
type Paths struct {
	Home       string
	ConfigDir  string // ~/.config/chat-archive (0700)
	ConfigFile string // config.json
	TokenFile  string // token (0600)
	StateDir   string // ~/.local/state/chat-archive (0700)
	StateDB    string // state.sqlite
	LockFile   string // lock
}

func pathsFor(home string) Paths {
	cfg := filepath.Join(home, ".config", "chat-archive")
	st := filepath.Join(home, ".local", "state", "chat-archive")
	return Paths{
		Home:       home,
		ConfigDir:  cfg,
		ConfigFile: filepath.Join(cfg, "config.json"),
		TokenFile:  filepath.Join(cfg, "token"),
		StateDir:   st,
		StateDB:    filepath.Join(st, "state.sqlite"),
		LockFile:   filepath.Join(st, "lock"),
	}
}

// DefaultRoots are the three default roots under home: Claude Code projects, Codex sessions and skills.
func DefaultRoots(home string) map[string]string {
	return map[string]string{
		RootClaudeProjects: filepath.Join(home, ".claude", "projects"),
		RootCodexSessions:  filepath.Join(home, ".codex", "sessions"),
		RootClaudeSkills:   filepath.Join(home, ".claude", "skills"),
	}
}

// Config is ~/.config/chat-archive/config.json, written by enroll. Unknown fields are ignored, so a config
// written by another version still loads.
type Config struct {
	ArchiveURL string            `json:"archive_url"`
	SourceID   string            `json:"source_id"`
	MachineID  string            `json:"machine_id"`
	Label      string            `json:"label"`
	Roots      map[string]string `json:"roots"`
}

var errNotEnrolled = errors.New("not enrolled: run `chat-archive-import enroll` first")

func loadConfig(p Paths) (Config, error) {
	b, err := os.ReadFile(p.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, errNotEnrolled
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("config.json: %w", err)
	}
	if len(c.Roots) == 0 {
		c.Roots = DefaultRoots(p.Home)
	}
	return c, nil
}

func saveConfig(p Paths, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.ConfigDir, p.ConfigFile, append(b, '\n'))
}

func loadToken(p Paths) (string, error) {
	b, err := os.ReadFile(p.TokenFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", errNotEnrolled
	}
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errNotEnrolled
	}
	return t, nil
}

func saveToken(p Paths, token string) error {
	return writeFileAtomic(p.ConfigDir, p.TokenFile, []byte(token+"\n"))
}

// writeFileAtomic writes name (0600) in dir (created 0700) through a temp file and rename.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// randomHex returns n random bytes as lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on linux and darwin
	}
	return hex.EncodeToString(b)
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
