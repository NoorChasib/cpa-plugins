package client_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import (
	"context"
	"net/http"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/client"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var (
	_ time.Duration = client.ConnectTimeout
	_ time.Duration = client.CommitTimeout
	_ time.Duration = client.PutPieceTimeout

	_ = client.Options{HTTPClient: (*http.Client)(nil), UserAgent: "", OnServerState: func(wire.ServerState) {}}

	_ func(string, string, client.Options) *client.Client                                                                               = client.New
	_ func(context.Context, string, wire.EnrollRequest) (wire.EnrollResponse, error)                                                    = client.Enroll
	_ func(*client.Client, context.Context) (wire.SourcesSelf, error)                                                                   = (*client.Client).SourcesSelf
	_ func(*client.Client, context.Context, []wire.Hash) ([]wire.Hash, []wire.Hash, wire.ServerState, error)                            = (*client.Client).Need
	_ func(*client.Client, context.Context, wire.Hash, wire.PieceKind, string, []byte) error                                            = (*client.Client).PutPiece
	_ func(*client.Client, context.Context, uint64, string, []wire.BatchPiece, [][]byte, []wire.Tombstone) (wire.CommitResponse, error) = (*client.Client).Commit
	_ func(*client.Client, context.Context, wire.Heartbeat) (wire.HeartbeatResponse, error)                                             = (*client.Client).Heartbeat
	_ func(*client.Client, context.Context, wire.MediaFetch) error                                                                      = (*client.Client).MediaFetch
	_ func(*client.Client, context.Context, []string) (map[string]wire.Coverage, error)                                                 = (*client.Client).SessionCoverage
	_ func(*client.Client, context.Context, string, wire.CallerRequest) error                                                           = (*client.Client).PutCaller

	// Additions.
	_ func(*client.Client, context.Context, wire.Hash, wire.PieceKind, string, []byte) error = (*client.Client).PutPieceZstd

	_ error = client.ErrUnauthorized
	_ error = client.ErrMalformed
	_ error = client.ErrTooLarge
	_ error = client.ErrUnsupportedProtocol
	_ error = &client.EpochChangedError{Epoch: 0, ArchiveID: ""}
	_ error = &client.RetryAfterError{After: time.Duration(0)}
	_ error = &client.StatusError{StatusCode: 0, Code: "", Rules: []string(nil)}
)
