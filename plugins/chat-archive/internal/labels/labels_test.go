package labels

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
)

type fakeHost struct {
	mu    sync.Mutex
	gets  int
	lists int
}

func (f *fakeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	f.mu.Lock()
	f.lists++
	f.mu.Unlock()
	return []protocol.HostAuthFileEntry{{AuthIndex: "a1", Email: "user@example.invalid", Label: "user@example.invalid", Provider: "claude"}}, nil
}

func (f *fakeHost) GetRuntime(_ context.Context, idx string) (protocol.HostAuthFileEntry, error) {
	f.mu.Lock()
	f.gets++
	f.mu.Unlock()
	if idx == "cfg" {
		return protocol.HostAuthFileEntry{}, errors.New("auth runtime info not found")
	}
	return protocol.HostAuthFileEntry{AuthIndex: idx, ProjectID: "proj-x"}, nil
}

func TestLoopFillsCacheAndLabels(t *testing.T) {
	c := New(map[string]string{"pinned": "Pinned label"})
	h := &fakeHost{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx, h); close(done) }()
	defer func() { cancel(); <-done }()
	wait := func(cond func() bool) {
		deadline := time.Now().Add(3 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(func() bool { _, _, ok := c.Lookup("a1"); return ok })
	if e, p, ok := c.Lookup("a1"); !ok || e != "user@example.invalid" || p != "" {
		t.Fatalf("lookup a1 = %q %q %v", e, p, ok)
	}
	c.Want("v1")
	c.Want("cfg")
	wait(func() bool { _, ok := c.Get("cfg"); return ok })
	if _, p, ok := c.Lookup("v1"); !ok || p != "proj-x" {
		t.Fatalf("lookup v1 = %q %v", p, ok)
	}
	if l := c.Label("cfg", "k1:"+"abcdef0123456789", "openai", "https://api.example.invalid/v1", "apikey"); l != "openai-apikey@api.example.invalid#abcdef" {
		t.Fatalf("config key label = %q", l)
	}
	if l := c.Label("pinned", "", "claude", "", "oauth"); l != "Pinned label" {
		t.Fatalf("pinned label = %q", l)
	}
	if l := c.Label("a1", "k1:x", "claude", "", "oauth"); l != "user@example.invalid" {
		t.Fatalf("runtime label = %q", l)
	}
}
