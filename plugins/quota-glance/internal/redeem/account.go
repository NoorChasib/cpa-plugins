package redeem

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// accountIDFromIDToken recovers the ChatGPT account id from the id_token's
// claims, for credential documents that carry the token but not the id.
//
// It mirrors quota-cache's own resolution so the two plugins agree on which
// account a credential belongs to; disagreeing would mean spending a credit on
// a different account than the card reported it for. The signature is not
// verified and does not need to be: this is reading a value out of a document
// CPA already holds, to address a request that CPA will authenticate.
//
// No part of the token is returned, logged, or kept.
func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if id := claimString(claims, "https://api.openai.com/auth.chatgpt_account_id"); id != "" {
		return id
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id := claimString(auth, "chatgpt_account_id"); id != "" {
			return id
		}
	}
	return claimString(claims, "chatgpt_account_id")
}

func claimString(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return value
}
