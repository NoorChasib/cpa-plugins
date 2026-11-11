package clienttest_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import (
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client/clienttest"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var (
	_ = clienttest.Options{ArchiveID: "", Epoch: uint64(0), Limits: wire.Limits{}}
	_ = clienttest.Received{SourceID: "", Envelope: wire.Envelope{}, Raw: []byte(nil), Status: wire.StatusOK, Reason: wire.Reason("")}
	_ = func(s *clienttest.Server) string { return s.URL }

	_ func(clienttest.Options) *clienttest.Server                        = clienttest.NewServer
	_ func(*clienttest.Server)                                           = (*clienttest.Server).Close
	_ func(*clienttest.Server, string, string) (string, string)          = (*clienttest.Server).AddSource
	_ func(*clienttest.Server) string                                    = (*clienttest.Server).NewEnrollCode
	_ func(*clienttest.Server, uint64)                                   = (*clienttest.Server).SetEpoch
	_ func(*clienttest.Server, string)                                   = (*clienttest.Server).SetArchiveID
	_ func(*clienttest.Server, uint32, []wire.Hash, []wire.Hash)         = (*clienttest.Server).SetExact
	_ func(*clienttest.Server, string, wire.Coverage)                    = (*clienttest.Server).SetCoverage
	_ func(*clienttest.Server, string, wire.Reason)                      = (*clienttest.Server).RejectKey
	_ func(*clienttest.Server, string)                                   = (*clienttest.Server).AcceptKey
	_ func(*clienttest.Server, string, int, time.Duration)               = (*clienttest.Server).FailNext
	_ func(*clienttest.Server, string) []clienttest.Received             = (*clienttest.Server).Envelopes
	_ func(*clienttest.Server, string) []wire.Tombstone                  = (*clienttest.Server).Tombstones
	_ func(*clienttest.Server, wire.Hash) (wire.PieceKind, []byte, bool) = (*clienttest.Server).Piece
	_ func(*clienttest.Server, string) []wire.Heartbeat                  = (*clienttest.Server).Heartbeats
	_ func(*clienttest.Server, string) []wire.MediaFetch                 = (*clienttest.Server).MediaFetches
	_ func(*clienttest.Server) map[string]wire.CallerRequest             = (*clienttest.Server).Callers
	_ func(*clienttest.Server, string, string) uint64                    = (*clienttest.Server).HW
	_ func(*clienttest.Server) [][]byte                                  = (*clienttest.Server).RequestBodies

	// Additions.
	_ func(*clienttest.Server, ...wire.Hash)  = (*clienttest.Server).MarkStaged
	_ func(*clienttest.Server, string) func() = (*clienttest.Server).Hold
	_ func(*clienttest.Server, wire.Limits)   = (*clienttest.Server).SetLimits
)
