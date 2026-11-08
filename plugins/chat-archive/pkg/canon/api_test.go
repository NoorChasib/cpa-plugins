package canon_test

// Compile-only freeze of the exported API. Every producer and the server build against it, so a
// signature, field or JSON tag here never changes or disappears; new identifiers are only added.

import (
	"context"
	"encoding/json"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

var (
	_ string = canon.Version
	_        = []string{canon.FormatClaude, canon.FormatOpenAIChat, canon.FormatOpenAIResponses, canon.FormatGemini,
		canon.FormatInteractions, canon.FormatMediaPrefix, canon.FormatMultipart, canon.FormatOpaque}
	_ int = canon.MinAttachment
	_ int = canon.MaxGunzip

	_ func(any) ([]byte, error)                                       = canon.Marshal
	_ func([]byte) ([]byte, []string, error)                          = canon.Canonicalize
	_ func([]byte, *mask.Masker) ([]byte, mask.Hits, []string, error) = canon.CanonicalizeMasked
	_ func(wire.PieceKind, string, []byte) canon.Piece                = canon.NewPiece
	_                                                                 = []string{canon.AttKey, canon.OpaqueKey}
	_                                                                 = []any{canon.Piece{}, canon.AttStub{}, canon.OpaqueStub{}}

	_                                                                                     = []any{canon.Input{}, canon.Options{}, canon.MediaURL{}, canon.Split{}}
	_ func(*canon.Split) wire.Body                                                        = (*canon.Split).Body
	_ func(*canon.Split, int) []wire.Hash                                                 = (*canon.Split).Refs
	_ func(context.Context, canon.Input, canon.Options) (*canon.Split, error)             = canon.SplitRequest
	_ func(context.Context, canon.Input, canon.Options) (*canon.Split, error)             = canon.SplitResponse
	_ func(context.Context, string, json.RawMessage, canon.Options) (*canon.Split, error) = canon.SplitMessages
	_ func([]byte, *mask.Masker) ([]byte, []canon.MediaURL, error)                        = canon.StripMediaURLs

	_ func(wire.Item) wire.Hash     = canon.ItemID
	_ func([]wire.Item) []wire.Hash = canon.ItemIDs
	_ func([]wire.Hash) wire.Hash   = canon.ListSHA
	_ func(a, b []wire.Hash) int    = canon.LCP

	_ = []canon.Role{canon.RoleSystem, canon.RoleToolSchema, canon.RoleParams, canon.RoleReminder, canon.RoleArchiveMCP,
		canon.RoleThinking, canon.RoleToolInput, canon.RoleToolResult, canon.RoleFailureBody, canon.RoleText}
	_                                                                                                              = []canon.IdxPolicy{canon.IdxNone, canon.IdxFull, canon.IdxHeadTail}
	_ func(canon.Role) canon.IdxPolicy                                                                             = canon.Role.IdxPolicy
	_                                                                                                              = []string{canon.ArchiveMCPMarker, canon.DefaultArchiveToolPrefix, canon.ReminderPrefix}
	_ int                                                                                                          = canon.ArchiveMCPMarkerWindow
	_ func(wire.Kind, json.RawMessage, []string, func(wire.Hash) ([]byte, bool)) (map[wire.Hash]canon.Role, error) = canon.PieceRoles
	_ func([]wire.Header) bool                                                                                     = canon.IsClaudeCode

	// Additions.
	_ func(wire.PieceKind, string, []byte, *mask.Masker) ([]mask.Finding, error)            = canon.DetectPiece
	_ error                                                                                 = canon.ErrUndecodableBody
	_ error                                                                                 = canon.ErrBodyBudget
	_                                                                                       = canon.Options{MaxDecoded: int64(0), MaxNodes: 0}
	_ func(context.Context, []byte, *mask.Masker, int) ([]byte, mask.Hits, []string, error) = canon.CanonicalizeMaskedContext
)

// memoShape pins the Memo method set.
type memoShape struct{}

func (memoShape) Get(wire.Hash) (string, mask.Hits, bool) { return "", nil, false }
func (memoShape) Add(wire.Hash, string, mask.Hits)        {}

var _ canon.Memo = memoShape{}
