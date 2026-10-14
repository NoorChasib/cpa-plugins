package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

func TestWarmupRefreshRespectsSpacingAndPollBackoff(t *testing.T) {
	c, fetcher, options := fixture(t)
	key := client.Key("claude", "one")
	if err := c.Step(context.Background(), fetcher.now); err != nil {
		t.Fatal(err)
	}
	if !c.RequestPoll(key, fetcher.now) {
		t.Fatal("targeted refresh refused")
	}
	if err := c.Step(context.Background(), fetcher.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 {
		t.Fatal("warm-up refresh bypassed global request spacing")
	}
	fetcher.failure = errors.New("provider unavailable")
	if err := c.Step(context.Background(), fetcher.now.Add(options.Spacing)); err != nil {
		t.Fatal(err)
	}
	if c.RequestPoll(key, fetcher.now.Add(options.Spacing)) {
		t.Fatal("warm-up refresh bypassed failed-poll backoff")
	}
	if c.RoundRemaining() != 0 {
		t.Fatal("one warm-up started an all-account reset round")
	}
}
