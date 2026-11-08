package mask_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import (
	"context"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
)

var (
	_ string                                                       = mask.Version
	_ func() string                                                = mask.RulesetSHA256
	_ func(mask.Config) (*mask.Masker, error)                      = mask.New
	_ func(*mask.Masker, string) (string, mask.Hits)               = (*mask.Masker).String
	_ func(*mask.Masker, string) []mask.Finding                    = (*mask.Masker).Detect
	_ func(*mask.Masker) uint32                                    = (*mask.Masker).ExactRev
	_ func(string) (func(mask.Config) (*mask.Masker, error), bool) = mask.Get

	// Additions.
	_ func(*mask.Masker, context.Context, string) (string, mask.Hits, error) = (*mask.Masker).StringContext
	_ func(*mask.Masker, string, map[string]time.Duration)                   = (*mask.Masker).ProfileRules

	// The importer's mask-dry-run finds ProfileRules through this interface.
	_ interface {
		ProfileRules(s string, acc map[string]time.Duration)
	} = (*mask.Masker)(nil)

	_ = mask.Config{ExactFPs: [][32]byte{}, CallerFPs: [][32]byte{}, ExactRev: uint32(0)}
	_ = mask.Finding{RuleID: "", Span: 0}
	_ = mask.Hits(map[string]uint32{})
)
