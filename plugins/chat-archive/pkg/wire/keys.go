package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// MaxKeyLen is the longest valid envelope key in bytes.
const MaxKeyLen = 200

// ValidKey reports whether key is 1 to MaxKeyLen bytes of ASCII [A-Za-z0-9_./:@-].
func ValidKey(key string) bool {
	if len(key) == 0 || len(key) > MaxKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '_', c == '.', c == '/', c == ':', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// KeySHA256 returns the sha256 of a key's bytes, the form in which Envelope.PriorKeySHA256 names an
// earlier key.
func KeySHA256(key string) Hash {
	return sha256.Sum256([]byte(key))
}

// The key builders below return the envelope key layouts. They do not validate their arguments;
// callers check the result with ValidKey.

// ExchangeBeginKey returns "ex/<request_id>/begin/<input_seq>".
func ExchangeBeginKey(requestID string, inputSeq uint32) string {
	return "ex/" + requestID + "/begin/" + strconv.FormatUint(uint64(inputSeq), 10)
}

// ExchangeAttemptKey returns "ex/<request_id>/attempt/<ordinal>".
func ExchangeAttemptKey(requestID string, ordinal uint32) string {
	return "ex/" + requestID + "/attempt/" + strconv.FormatUint(uint64(ordinal), 10)
}

// ExchangeResponseKey returns "ex/<request_id>/response/<index>".
func ExchangeResponseKey(requestID string, index uint32) string {
	return "ex/" + requestID + "/response/" + strconv.FormatUint(uint64(index), 10)
}

// ExchangeCompletionKey returns "ex/<request_id>/completion".
func ExchangeCompletionKey(requestID string) string {
	return "ex/" + requestID + "/completion"
}

// ExchangeCompletionKeyAt returns "ex/<request_id>/completion/<started_at as unix ns, decimal>": the key
// of a completion whose RequestID CPA reused for another lifecycle (an in-place WebSocket failover
// since CPA v8.0.76). started_at is unique per lifecycle of one RequestID, so the keys never collide.
// "ex/<rid>/completion" sorts before every one of them, and 19-digit values sort in time order.
func ExchangeCompletionKeyAt(requestID string, startedAt time.Time) string {
	return "ex/" + requestID + "/completion/" + strconv.FormatInt(startedAt.UnixNano(), 10)
}

// UsageKey returns "usage/<usage_request_id>".
func UsageKey(usageRequestID string) string {
	return "usage/" + usageRequestID
}

// ClaudeUUIDKey returns "cc/u/<sessionId>/<uuid>/<sha256>" for a Claude Code record with a uuid.
// sha is RecordKeyHash's result.
func ClaudeUUIDKey(sessionID, recordUUID, sha string) string {
	return "cc/u/" + sessionID + "/" + recordUUID + "/" + sha
}

// ClaudeNoUUIDKey returns "cc/n/<sessionId>/<type>/<sha256>" for a Claude Code record without a uuid.
// sha is RecordKeyHash's result.
func ClaudeNoUUIDKey(sessionID, recordType, sha string) string {
	return "cc/n/" + sessionID + "/" + recordType + "/" + sha
}

// JournalKey returns "cc/j/<runId>/<agentId or "-">/<type>/<sha256>" for a workflow journal record;
// an empty agentID becomes "-". sha is RecordKeyHash's result.
func JournalKey(runID, agentID, recordType, sha string) string {
	if agentID == "" {
		agentID = "-"
	}
	return "cc/j/" + runID + "/" + agentID + "/" + recordType + "/" + sha
}

// CodexKey returns "cx/<thread_id>/<ordinal>/<sha256>" for a Codex rollout record.
// sha is RecordKeyHash's result.
func CodexKey(threadID string, ordinal int64, sha string) string {
	return "cx/" + threadID + "/" + strconv.FormatInt(ordinal, 10) + "/" + sha
}

// ImportFileKey returns "f/<client>/<hex(sha256(file_key))[:32]>/<content sha256>".
func ImportFileKey(client, fileKey string, content Hash) string {
	fk := sha256.Sum256([]byte(fileKey))
	return "f/" + client + "/" + hex.EncodeToString(fk[:])[:32] + "/" + content.String()
}

// RecordKeyHash returns the 64 lowercase hex of sha256(canon(Frame ⧺ item ids ⧺ extra)), the
// <sha256> segment of every import.record key. frame is ImportRecord.Frame (already
// canon/v1), itemIDs are canon.ItemIDs(ImportRecord.Items), extra is ImportRecord.Extra (nil = absent).
//
// The hashed bytes are the canon/v1 JSON array [<frame>, "<hex id>"..., "<hex extra>"], with the
// extra element omitted when extra is nil. pkg/wire cannot import pkg/canon (canon imports wire), and
// every element is already canonical, so the array is assembled byte for byte.
// An empty frame is written as null, the JSON a nil json.RawMessage marshals to.
func RecordKeyHash(frame json.RawMessage, itemIDs []Hash, extra *Hash) string {
	h := sha256.New()
	h.Write([]byte{'['})
	if len(frame) == 0 {
		h.Write([]byte("null"))
	} else {
		h.Write(frame)
	}
	var buf [2 + 64 + 1]byte // ,"<64 hex>"
	writeID := func(id Hash) {
		buf[0], buf[1] = ',', '"'
		hex.Encode(buf[2:66], id[:])
		buf[66] = '"'
		h.Write(buf[:67])
	}
	for _, id := range itemIDs {
		writeID(id)
	}
	if extra != nil {
		writeID(*extra)
	}
	h.Write([]byte{']'})
	return hex.EncodeToString(h.Sum(nil))
}
