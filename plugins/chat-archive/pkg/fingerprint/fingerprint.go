// Package fingerprint holds the identity hashes every producer and the server must compute
// identically: CPA's CallerScope, the k1 HMAC, source-token hashes and exact-value fingerprints.
// It is pure Go and imports nothing from this module.
//
// The exported API is frozen: api_test.go pins every signature, and an exported identifier, field
// or JSON tag is never changed or removed, only added.
package fingerprint

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	callerScopePrefix = "cli-proxy-api:caller-scope:v1\x00"
	tokenPrefix       = "chat-archive/token/v1\x00"
	exactPrefix       = "chat-archive/exact/v1\x00"
)

// CallerScope returns hex(sha256("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(key))) as 64
// lowercase hex, or "" when the trimmed key is empty. It equals CPA's CallerScope
// (sdk/cliproxy/session/identity.go:210-217).
func CallerScope(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(callerScopePrefix + key))
	return hex.EncodeToString(sum[:])
}

// HMAC returns keyID + ":" + hex(HMAC-SHA256(key, strings.Join(parts, "|"))), for example
// account_fp = HMAC("k1", key, provider, Source).
func HMAC(keyID string, key []byte, parts ...string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strings.Join(parts, "|")))
	return keyID + ":" + hex.EncodeToString(mac.Sum(nil))
}

// TokenHash returns sha256("chat-archive/token/v1\x00" + token), the stored form of a source token.
func TokenHash(token string) [32]byte {
	return sha256.Sum256([]byte(tokenPrefix + token))
}

// ExactFP returns sha256("chat-archive/exact/v1\x00" + value), the exact-value fingerprint.
func ExactFP(value string) [32]byte {
	return sha256.Sum256([]byte(exactPrefix + value))
}
