package capture

import (
	"strings"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/protocol"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/canon"
)

// Exchange kinds (wire.ExchangeBegin.Kind).
const (
	KindChat        = "chat"
	KindCountTokens = "count_tokens"
	KindCompaction  = "compaction"
	KindPrewarm     = "prewarm"
	KindImage       = "image"
	KindVideoCreate = "video_create"
	KindVideoPoll   = "video_poll"
	KindSpeech      = "speech"
	KindOpaque      = "opaque"
)

// Classification is the request kind and transport recorded on one begin.
type Classification struct {
	Kind      string
	Transport string // http | ws
	Legacy    bool   // /v1/completions
}

var chatFormats = map[string]bool{"claude": true, "openai": true, "openai-response": true, "codex": true, "gemini": true, "interactions": true}

// Classify derives the kind from SourceFormat, request_path, generate and
// execution_session_id. Gemini count_tokens cannot be told from generateContent by path (the
// route pattern is /v1beta/models/*action); its begin stays chat and derive reclassifies it from
// the response shape.
func Classify(sourceFormat string, m protocol.Meta) Classification {
	c := Classification{Transport: "http"}
	if m.ExecutionSessionID != "" {
		c.Transport = "ws"
	}
	p := m.RequestPath
	switch {
	case strings.HasSuffix(p, "/messages/count_tokens"):
		c.Kind = KindCountTokens
	case strings.HasSuffix(p, "/responses/compact"):
		c.Kind = KindCompaction
	case m.Generate != nil && !*m.Generate:
		c.Kind = KindPrewarm
	case strings.Contains(p, "/images/"):
		c.Kind = KindImage
	case strings.Contains(p, "/videos"):
		if strings.Contains(p, ":") || sourceFormat == "openai-video" && strings.Contains(p, "/content") {
			c.Kind = KindVideoPoll
		} else {
			c.Kind = KindVideoCreate
		}
	case strings.Contains(p, "/audio/speech"), strings.HasSuffix(p, "/tts"):
		c.Kind = KindSpeech
	case sourceFormat == "openai-image":
		c.Kind = KindImage
	case sourceFormat == "openai-video":
		c.Kind = KindVideoCreate
	case sourceFormat == "openai-speech":
		c.Kind = KindSpeech
	case chatFormats[sourceFormat]:
		c.Kind = KindChat
	default:
		c.Kind = KindOpaque
	}
	c.Legacy = strings.HasSuffix(p, "/v1/completions")
	return c
}

// CanonFormat maps CPA's SourceFormat to the canon Body.Format the split applies.
func CanonFormat(sourceFormat string) string {
	switch sourceFormat {
	case "claude":
		return canon.FormatClaude
	case "openai":
		return canon.FormatOpenAIChat
	case "openai-response", "codex":
		return canon.FormatOpenAIResponses
	case "gemini":
		return canon.FormatGemini
	case "interactions":
		return canon.FormatInteractions
	case "openai-image":
		return canon.FormatMediaPrefix + "images"
	case "openai-video":
		return canon.FormatMediaPrefix + "videos"
	case "openai-speech":
		return canon.FormatMediaPrefix + "speech"
	default:
		return canon.FormatOpaque
	}
}
