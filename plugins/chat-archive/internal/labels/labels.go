// Package labels is the background label loop: host.auth.list every 10 minutes
// and host.auth.get_runtime per new AuthIndex, through the host-call gate. usage.handle reads only
// the in-memory cache; it never calls the host. Personal fields other than email and project id
// are never decoded (see protocol.HostAuthFileEntry).
package labels

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/safego"
)

// Host is the host-call bridge (abi.go). Never host.auth.get: it returns credential JSON.
type Host interface {
	ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error)
	GetRuntime(ctx context.Context, authIndex string) (protocol.HostAuthFileEntry, error)
}

// Intervals.
var (
	ListInterval = 10 * time.Minute
	CallTimeout  = 10 * time.Second
)

const maxEntries = 4096

// Entry is one cached credential.
type Entry struct {
	Email, ProjectID, Label, Provider, BaseURL, Type string
	RuntimeOnly                                      bool
	Missing                                          bool // get_runtime had no entry (config API key)
}

// Cache is the label cache. It is safe for concurrent use.
type Cache struct {
	mu     sync.RWMutex
	m      map[string]Entry
	want   chan string
	config map[string]string // account-labels
}

// New returns an empty cache with the configured account-labels.
func New(config map[string]string) *Cache {
	return &Cache{m: map[string]Entry{}, want: make(chan string, 256), config: config}
}

// Lookup implements hooks.LabelCache.
func (c *Cache) Lookup(authIndex string) (string, string, bool) {
	c.mu.RLock()
	e, ok := c.m[authIndex]
	c.mu.RUnlock()
	return e.Email, e.ProjectID, ok
}

// Want implements hooks.LabelCache; it never blocks.
func (c *Cache) Want(authIndex string) {
	select {
	case c.want <- authIndex:
	default:
	}
}

// Get returns the cached entry.
func (c *Cache) Get(authIndex string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[authIndex]
	return e, ok
}

func (c *Cache) put(idx string, e Entry) {
	if idx == "" {
		return
	}
	c.mu.Lock()
	if len(c.m) >= maxEntries {
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[idx] = e
	c.mu.Unlock()
}

func entryOf(f protocol.HostAuthFileEntry) Entry {
	return Entry{Email: f.Email, ProjectID: f.ProjectID, Label: f.Label, Provider: f.Provider, BaseURL: f.BaseURL, Type: f.Type, RuntimeOnly: f.RuntimeOnly}
}

// Label returns the account label for a usage record: the account-labels
// entry for the AuthIndex or the account fingerprint, else for config API keys
// "<provider>-apikey@<BaseURL host>#<fp[:6]>", else the runtime label.
func (c *Cache) Label(authIndex, accountFP, provider, baseURL, authType string) string {
	if l, ok := c.config[authIndex]; ok && authIndex != "" {
		return l
	}
	if l, ok := c.config[accountFP]; ok && accountFP != "" {
		return l
	}
	e, ok := c.Get(authIndex)
	if ok && !e.Missing && e.Label != "" && authType != "apikey" {
		return e.Label
	}
	if authType == "apikey" || (ok && e.Missing) {
		host := ""
		if u, err := url.Parse(baseURL); err == nil {
			host = u.Hostname()
		}
		fp := accountFP
		if _, hexPart, ok := strings.Cut(fp, ":"); ok {
			fp = hexPart // "k1:<hex>" → "<hex>"
		}
		if len(fp) > 6 {
			fp = fp[:6]
		}
		if provider == "" {
			provider = "unknown"
		}
		return provider + "-apikey@" + host + "#" + fp
	}
	if ok {
		return e.Label
	}
	return ""
}

// Run is the label loop. It lists every ListInterval and resolves wanted AuthIndexes.
func (c *Cache) Run(ctx context.Context, host Host) {
	safego.Checkpoint("labels")
	if host == nil {
		<-ctx.Done()
		return
	}
	list := func() {
		lctx, cancel := context.WithTimeout(ctx, CallTimeout)
		defer cancel()
		files, err := host.ListAuth(lctx)
		if err != nil {
			return
		}
		for _, f := range files {
			c.put(f.AuthIndex, entryOf(f))
		}
	}
	list()
	t := time.NewTicker(ListInterval)
	defer t.Stop()
	for {
		safego.Checkpoint("labels")
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			list()
		case idx := <-c.want:
			if _, ok := c.Get(idx); ok {
				continue
			}
			gctx, cancel := context.WithTimeout(ctx, CallTimeout)
			f, err := host.GetRuntime(gctx, idx)
			cancel()
			if err != nil {
				c.put(idx, Entry{Missing: true})
				continue
			}
			c.put(idx, entryOf(f))
		}
	}
}
