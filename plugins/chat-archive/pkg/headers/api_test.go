package headers_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import (
	"net/http"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/headers"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var (
	_ func(http.Header, *mask.Masker, string, []byte) ([]wire.Header, wire.ArchiveHints, []string) = headers.FilterRequest
	_ func(http.Header, *mask.Masker) []wire.Header                                                = headers.FilterResponse
	_ func(string, *mask.Masker) string                                                            = headers.StripURL
	_ func(http.Header) []string                                                                   = headers.ArchiveHeaderNames
)
