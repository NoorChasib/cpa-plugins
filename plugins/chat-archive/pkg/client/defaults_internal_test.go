package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// TestDefaultTransport: no proxy (the source token never goes to an HTTP_PROXY), a connect timeout
// and a bound on the wait for response headers.
func TestDefaultTransport(t *testing.T) {
	tr, ok := defaultHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("default transport is not *http.Transport")
	}
	if tr.Proxy != nil {
		t.Error("default transport must not use a proxy")
	}
	if tr.ResponseHeaderTimeout != CommitTimeout {
		t.Errorf("ResponseHeaderTimeout = %v", tr.ResponseHeaderTimeout)
	}
}

// TestDefaultDeadlines: against a server that accepts and never answers, routes without their own
// deadline return once the default one passes; a caller deadline still wins.
func TestDefaultDeadlines(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // held open, never answered
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	oldRoute, oldNeed := routeTimeout, needTimeout
	routeTimeout, needTimeout = 150*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { routeTimeout, needTimeout = oldRoute, oldNeed })

	c := New("http://"+ln.Addr().String(), "cha_src_synthetic", Options{})
	bg := context.Background()
	for name, call := range map[string]func() error{
		"heartbeat":    func() error { _, err := c.Heartbeat(bg, wire.Heartbeat{}); return err },
		"sources-self": func() error { _, err := c.SourcesSelf(bg); return err },
		"media-fetch":  func() error { return c.MediaFetch(bg, wire.MediaFetch{}) },
		"coverage":     func() error { _, err := c.SessionCoverage(bg, nil); return err },
		"put-caller":   func() error { return c.PutCaller(bg, "fp", wire.CallerRequest{}) },
		"enroll":       func() error { _, err := Enroll(bg, "http://"+ln.Addr().String(), wire.EnrollRequest{}); return err },
		"need":         func() error { _, _, _, err := c.Need(bg, nil); return err },
	} {
		start := time.Now()
		err := call()
		d := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) || d > 2*time.Second {
			t.Errorf("%s: err %v after %v", name, err, d)
		}
		if name == "need" && d < 250*time.Millisecond {
			t.Errorf("need used the %v route default, want its own", d)
		}
	}
	// A shorter caller deadline wins.
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.SourcesSelf(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 140*time.Millisecond {
		t.Errorf("caller deadline: err %v after %v", err, time.Since(start))
	}
}
