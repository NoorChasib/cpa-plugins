package wire_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import (
	"encoding"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var (
	_ int    = wire.ProtocolV1
	_ string = wire.CanonV1
	_ string = wire.MaskV1

	_ encoding.TextMarshaler          = wire.Hash{}
	_ encoding.TextUnmarshaler        = (*wire.Hash)(nil)
	_ func(wire.Hash) string          = wire.Hash.String
	_ func(string) (wire.Hash, error) = wire.ParseHash

	_ wire.PieceKind                       = wire.PieceJSON
	_ wire.PieceKind                       = wire.PieceText
	_ wire.PieceKind                       = wire.PieceAttachment
	_ func(wire.PieceKind) string          = wire.PieceKind.String
	_ func(string) (wire.PieceKind, error) = wire.ParsePieceKind

	_ = []wire.Kind{wire.KindExchangeBegin, wire.KindExchangeAttempt, wire.KindExchangeResponse,
		wire.KindExchangeDone, wire.KindUsage, wire.KindImportRecord, wire.KindImportFile}
	_ func(wire.Kind) bool = wire.Kind.SourceScoped

	_ = []string{wire.TombRejectedLocal, wire.TombRejectedTooLarge, wire.TombMissingRef}
	_ = []string{wire.FlagDupKeys, wire.FlagBadUTF8, wire.FlagArgsRaw, wire.FlagGunzipped,
		wire.FlagLegacyCompletions, wire.FlagInputParseError}
	_ = []string{wire.DroppedBodyBudget, wire.DroppedMaskTimeout, wire.DroppedOversize,
		wire.DroppedParseError, wire.DroppedDiskGuard}

	_ int                                                   = wire.MaxKeyLen
	_ func(string) bool                                     = wire.ValidKey
	_ func(string, uint32) string                           = wire.ExchangeBeginKey
	_ func(string, uint32) string                           = wire.ExchangeAttemptKey
	_ func(string, uint32) string                           = wire.ExchangeResponseKey
	_ func(string) string                                   = wire.ExchangeCompletionKey
	_ func(string) string                                   = wire.UsageKey
	_ func(string, string, string) string                   = wire.ClaudeUUIDKey
	_ func(string, string, string) string                   = wire.ClaudeNoUUIDKey
	_ func(string, string, string, string) string           = wire.JournalKey
	_ func(string, int64, string) string                    = wire.CodexKey
	_ func(string, string, wire.Hash) string                = wire.ImportFileKey
	_ func(json.RawMessage, []wire.Hash, *wire.Hash) string = wire.RecordKeyHash

	_ string                                     = wire.CAB1Magic
	_                                            = []byte{wire.RecEnvelope, wire.RecPiece, wire.RecTombstone, wire.RecEnd}
	_ int                                        = wire.MaxTombstoneJSON
	_ string                                     = wire.ContentTypeBatchV1
	_ error                                      = wire.ErrCAB1Framing
	_ error                                      = wire.ErrCAB1TooLarge
	_ func(io.Writer, wire.Batch) error          = wire.WriteCAB1
	_ func(io.Reader, int64) (wire.Batch, error) = wire.ReadCAB1
	_ int                                        = wire.CAB1RecordOverhead
	_ func(wire.Batch) int                       = wire.CAB1Size

	_ = []wire.Status{wire.StatusOK, wire.StatusDuplicate, wire.StatusConflict, wire.StatusRejected, wire.StatusRetry}
	_ = []wire.Reason{wire.ReasonMaskViolation, wire.ReasonMaskVersionMissing, wire.ReasonMaskVersionRetired,
		wire.ReasonCanonVersion, wire.ReasonSchema, wire.ReasonTooLarge,
		wire.ReasonMissingRef, wire.ReasonBusy, wire.ReasonStoreUnavailable}
	_ = []string{wire.ErrCodeEpochChanged, wire.ErrCodePieceHashMismatch, wire.ErrCodeUnsupportedProtocol}

	_ = []string{wire.PathHealthz, wire.PathSourcesSelf, wire.PathNeed, wire.PathPiecePrefix, wire.PathCommit,
		wire.PathHeartbeat, wire.PathMediaFetch, wire.PathSessionCoverage, wire.PathEnroll, wire.PathCallerPrefix}
	_                                            = []string{wire.HeaderEpoch, wire.HeaderArchiveID, wire.HeaderServerTime, wire.HeaderEpochSeen, wire.HeaderPieceKind}
	_ func(http.Header) (wire.ServerState, bool) = wire.ParseServerState
	_ int                                        = wire.MaxCoverageIDs
	_ int                                        = wire.MaxMediaFetchJSON

	_ wire.Limits   = wire.DefaultLimits
	_ int           = wire.TargetCommitBytes
	_ int           = wire.TargetCommitEnvelopes
	_ time.Duration = wire.TargetCommitLinger

	_ error                                         = wire.ErrUnsupportedProtocol
	_ error                                         = wire.ErrSchema
	_ func([]byte) (wire.Envelope, error)           = wire.DecodeEnvelope
	_ func(wire.Kind, json.RawMessage) (any, error) = wire.DecodeBody

	// Additions.
	_ func([]byte) (wire.Tombstone, error)   = wire.DecodeTombstone
	_ func(wire.Envelope) wire.Hash          = wire.Envelope.BodySHA256
	_ func(wire.Heartbeat) wire.HeartbeatRow = wire.Heartbeat.Row
	_                                        = wire.HeartbeatRow{EpochSeen: uint32(0), ComponentVersion: "", HostVersion: "",
		LastHookAt: time.Time{}, LastUsageAt: time.Time{}, OutboxDepth: uint32(0), OutboxBytes: uint64(0),
		OldestPendingS: uint32(0), Counters: map[string]uint64(nil), BootID: ""}

	// Every named type exists.
	_ = []any{
		wire.Envelope{}, wire.Locator{}, wire.Tombstone{}, wire.Item{}, wire.Base{}, wire.Body{}, wire.Header{},
		wire.ArchiveHints{}, wire.ExchangeBegin{}, wire.ExchangeAttempt{}, wire.StreamEvent{}, wire.ExchangeResponse{},
		wire.ExchangeCompletion{}, wire.MediaFetch{}, wire.Tokens{}, wire.UsageAttempt{}, wire.ImportRecord{},
		wire.ImportFile{}, wire.BatchPiece{}, wire.Batch{}, wire.CommitResponse{}, wire.CommitResult{},
		wire.ErrorBody{}, wire.ServerState{}, wire.NeedRequest{}, wire.NeedResponse{}, wire.SourcesSelf{},
		wire.Accepted{}, wire.MaskAcceptance{}, wire.ExactSet{}, wire.EnrollRequest{}, wire.EnrollResponse{},
		wire.CallerRequest{}, wire.CallerResponse{}, wire.CoverageRequest{}, wire.CoverageResponse{},
		wire.Coverage{}, wire.HeartbeatResponse{}, wire.Limits{}, wire.Heartbeat{}, wire.OutboxStats{},
		wire.UploaderStats{}, wire.RuntimeStats{}, wire.MaskerStats{}, wire.ImporterStats{},
	}
)
