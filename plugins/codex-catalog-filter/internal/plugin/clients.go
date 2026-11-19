package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/agy"
	"github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/claudelist"
)

const (
	// clientsFile holds the Gemini-format and Claude-format lists CPA last
	// returned, and hashes of client keys CPA accepted for the agy route. It
	// never holds a key, a request, or model instructions.
	clientsFile = "clients.json"
	// agyKeyWindow is how long a key CPA accepted may receive the last good
	// Antigravity list while CPA cannot answer.
	agyKeyWindow = 24 * time.Hour
	maxAgyKeys   = 64
	// clientsRewriteInterval matches seenRewriteInterval: unchanged content is
	// rewritten at most this often; changes are written at once.
	clientsRewriteInterval = time.Hour
	maxClientsFileBytes    = 4 << 20
)

type geminiSnapshot struct {
	FetchedAt time.Time   `json:"fetched_at"`
	Models    []agy.Model `json:"models"`
}

type claudeSnapshot struct {
	SeenAt time.Time          `json:"seen_at"`
	Models []claudelist.Entry `json:"models"`
}

type acceptedKey struct {
	Hash       string    `json:"hash"`
	AcceptedAt time.Time `json:"accepted_at"`
}

type clientsData struct {
	Version int             `json:"version"`
	Gemini  *geminiSnapshot `json:"gemini,omitempty"`
	Claude  *claudeSnapshot `json:"claude,omitempty"`
	AgyKeys []acceptedKey   `json:"agy_keys,omitempty"`
}

// agyKeyHash identifies an Authorization value without storing it. The fixed
// domain-separation prefix (not a secret salt) keeps it distinct from CPA's
// own caller_scope hash of the same key.
func agyKeyHash(authorization string) string {
	sum := sha256.Sum256([]byte("codex-catalog-filter/agy-key/v1\x00" + authorization))
	return hex.EncodeToString(sum[:])
}

// clientStore remembers what CPA listed to the Antigravity route and to Claude
// Code, in memory and best effort on disk, plus runtime status for /state.
type clientStore struct {
	mu        sync.Mutex
	dir       string
	gemini    *geminiSnapshot
	claude    *claudeSnapshot
	keys      map[string]time.Time
	writtenAt time.Time
	saveFails bool

	// Runtime only.
	agyServedAt   time.Time
	agyServedFrom string
	agyError      string
	agyErrorAt    time.Time
	claudeLists   uint64
	claudeRewrite uint64
	claudeLastAt  time.Time
}

// load reads the saved lists from dir the first time dir is configured. A
// newer list already in memory wins.
func (s *clientStore) load(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir == s.dir {
		return
	}
	s.dir, s.writtenAt, s.saveFails = dir, time.Time{}, false
	if dir == "" {
		return
	}
	saved, err := readClients(filepath.Join(dir, clientsFile))
	if err != nil {
		return
	}
	if saved.Gemini != nil && (s.gemini == nil || s.gemini.FetchedAt.Before(saved.Gemini.FetchedAt)) {
		s.gemini = saved.Gemini
	}
	if saved.Claude != nil && (s.claude == nil || s.claude.SeenAt.Before(saved.Claude.SeenAt)) {
		s.claude = saved.Claude
	}
	if s.keys == nil {
		s.keys = map[string]time.Time{}
	}
	for _, key := range saved.AgyKeys {
		if key.AcceptedAt.After(s.keys[key.Hash]) {
			s.keys[key.Hash] = key.AcceptedAt
		}
	}
	s.writtenAt = latest(saved)
}

func latest(saved *clientsData) time.Time {
	var at time.Time
	if saved.Gemini != nil {
		at = saved.Gemini.FetchedAt
	}
	if saved.Claude != nil && saved.Claude.SeenAt.After(at) {
		at = saved.Claude.SeenAt
	}
	return at
}

// rememberGemini records a list CPA returned to the agy route as the last
// good one, and that CPA accepted the request's key, when it carried one.
func (s *clientStore) rememberGemini(dir string, models []agy.Model, authorization string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.gemini == nil || !slices.EqualFunc(s.gemini.Models, models, sameModel)
	s.gemini = &geminiSnapshot{FetchedAt: at.UTC(), Models: models}
	changed = s.acceptKeyLocked(authorization, at) || changed
	s.saveLocked(dir, at, changed)
}

// acceptKey records that CPA accepted the request's key with a valid list
// that is not kept as the last good one.
func (s *clientStore) acceptKey(dir, authorization string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveLocked(dir, at, s.acceptKeyLocked(authorization, at))
}

// acceptKeyLocked reports whether the key was not accepted before.
func (s *clientStore) acceptKeyLocked(authorization string, at time.Time) bool {
	if authorization == "" {
		return false
	}
	if s.keys == nil {
		s.keys = map[string]time.Time{}
	}
	hash := agyKeyHash(authorization)
	_, known := s.keys[hash]
	s.keys[hash] = at.UTC()
	return !known
}

func sameModel(a, b agy.Model) bool {
	return a.ID == b.ID && a.DisplayName == b.DisplayName && a.Description == b.Description &&
		a.InputTokenLimit == b.InputTokenLimit && a.OutputTokenLimit == b.OutputTokenLimit && slices.Equal(a.OutputModalities, b.OutputModalities)
}

// forgetKey drops a key CPA rejected, so it can no longer receive the last
// good list.
func (s *clientStore) forgetKey(dir, authorization string, at time.Time) {
	if authorization == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := agyKeyHash(authorization)
	if _, known := s.keys[hash]; !known {
		return
	}
	delete(s.keys, hash)
	s.saveLocked(dir, at, true)
}

// lastGoodGemini returns the last list CPA returned to the agy route, only
// for a request whose Authorization CPA accepted with a 200 within
// agyKeyWindow. A request without Authorization never gets it.
func (s *clientStore) lastGoodGemini(authorization string, now time.Time) []agy.Model {
	if authorization == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	accepted, ok := s.keys[agyKeyHash(authorization)]
	if !ok || now.Sub(accepted) >= agyKeyWindow || s.gemini == nil {
		return nil
	}
	return s.gemini.Models
}

func (s *clientStore) agyServed(from string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agyServedAt, s.agyServedFrom = at.UTC(), from
}

func (s *clientStore) agyFailed(code string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agyError, s.agyErrorAt = code, at.UTC()
}

// rememberClaude records the de-cloaked list CPA returned to Claude Code.
func (s *clientStore) rememberClaude(dir string, models []claudelist.Entry, rewritten bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claudeLists++
	if rewritten {
		s.claudeRewrite++
	}
	s.claudeLastAt = at.UTC()
	changed := s.claude == nil || !slices.Equal(s.claude.Models, models)
	s.claude = &claudeSnapshot{SeenAt: at.UTC(), Models: models}
	s.saveLocked(dir, at, changed)
}

type clientsView struct {
	gemini        *geminiSnapshot
	claude        *claudeSnapshot
	saved         bool
	agyServedAt   time.Time
	agyServedFrom string
	agyError      string
	agyErrorAt    time.Time
	claudeLists   uint64
	claudeRewrite uint64
	claudeLastAt  time.Time
}

func (s *clientStore) view() clientsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clientsView{
		gemini: s.gemini, claude: s.claude, saved: !s.saveFails,
		agyServedAt: s.agyServedAt, agyServedFrom: s.agyServedFrom, agyError: s.agyError, agyErrorAt: s.agyErrorAt,
		claudeLists: s.claudeLists, claudeRewrite: s.claudeRewrite, claudeLastAt: s.claudeLastAt,
	}
}

// saveLocked writes the file when its content changed, after a failed write,
// or when the last write is older than clientsRewriteInterval.
func (s *clientStore) saveLocked(dir string, at time.Time, changed bool) {
	if dir == "" || dir != s.dir {
		return
	}
	if !changed && !s.saveFails && at.Sub(s.writtenAt) < clientsRewriteInterval {
		return
	}
	data := clientsData{Version: 1, Gemini: s.gemini, Claude: s.claude}
	for hash, accepted := range s.keys {
		if at.Sub(accepted) >= agyKeyWindow {
			delete(s.keys, hash)
			continue
		}
		data.AgyKeys = append(data.AgyKeys, acceptedKey{Hash: hash, AcceptedAt: accepted})
	}
	sort.Slice(data.AgyKeys, func(i, j int) bool { return data.AgyKeys[i].AcceptedAt.After(data.AgyKeys[j].AcceptedAt) })
	if len(data.AgyKeys) > maxAgyKeys {
		for _, dropped := range data.AgyKeys[maxAgyKeys:] {
			delete(s.keys, dropped.Hash)
		}
		data.AgyKeys = data.AgyKeys[:maxAgyKeys]
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err == nil {
		err = writeFileAtomic(dir, clientsFile, raw)
	}
	if err != nil {
		s.saveFails = true
		return
	}
	s.writtenAt, s.saveFails = at, false
}

func readClients(path string) (*clientsData, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxClientsFileBytes+1))
	if err != nil || len(raw) > maxClientsFileBytes {
		return nil, errors.New("unreadable client lists")
	}
	var saved clientsData
	if json.Unmarshal(raw, &saved) != nil || saved.Version != 1 {
		return nil, errors.New("invalid client lists")
	}
	if saved.Gemini != nil && !validGemini(saved.Gemini) {
		saved.Gemini = nil
	}
	if saved.Claude != nil && !validClaude(saved.Claude) {
		saved.Claude = nil
	}
	keys := saved.AgyKeys[:0]
	for _, key := range saved.AgyKeys {
		if len(key.Hash) == 64 && !key.AcceptedAt.IsZero() {
			if _, err := hex.DecodeString(key.Hash); err == nil {
				keys = append(keys, key)
			}
		}
	}
	saved.AgyKeys = keys
	return &saved, nil
}

func validGemini(g *geminiSnapshot) bool {
	if g.FetchedAt.IsZero() || len(g.Models) == 0 {
		return false
	}
	for _, m := range g.Models {
		if m.ID == "" {
			return false
		}
	}
	return true
}

func validClaude(c *claudeSnapshot) bool {
	if c.SeenAt.IsZero() || len(c.Models) == 0 {
		return false
	}
	for _, m := range c.Models {
		if m.ID == "" {
			return false
		}
	}
	return true
}
