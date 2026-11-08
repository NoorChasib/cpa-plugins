package fingerprint_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import "github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/fingerprint"

var (
	_ func(string) string                    = fingerprint.CallerScope
	_ func(string, []byte, ...string) string = fingerprint.HMAC
	_ func(string) [32]byte                  = fingerprint.TokenHash
	_ func(string) [32]byte                  = fingerprint.ExactFP
)
