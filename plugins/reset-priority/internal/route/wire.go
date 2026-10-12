package route

import (
	"bytes"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
)

// pickWire is the part of CPA's scheduler.pick request the router reads.
// CPA marshals pluginapi.SchedulerPickRequest directly, and that type has no
// JSON tags, so the keys are Go field names (CPA sdk/pluginapi/types.go).
//
// It has no Headers, Plugin, Model or candidate Metadata field, and
// Options.Metadata has a single field. encoding/json skips every other key
// without making a string of it, so the client's request headers, which can
// hold its CPA access key, and the rest of the round's metadata are never
// materialized.
type pickWire struct {
	Options struct {
		Metadata struct {
			// Only on a pick after a failed attempt in this round: CPA writes
			// the attempt's auth ID into the round's shared metadata before
			// each attempt.
			SelectedAuthID configID `json:"selected_auth_id"`
		}
	}
	Candidates []pickCandidate
}

// pickCandidate is one candidate CPA offers. CPA has already removed the
// attributes whose names suggest a secret, and never sets a candidate's
// Metadata.
type pickCandidate struct {
	ID         string
	Provider   string
	Attributes struct {
		AuthKind string `json:"auth_kind"`
		Source   string `json:"source"`
		// ConfigIndex is the key's position in CPA's Claude API key list,
		// counted from 0 across every api-keys.claude group in file order,
		// which is not a secret. It is kept on an exploration entry, after
		// validIndex, so the status can say which key is being explored without
		// naming it.
		ConfigIndex string `json:"config_index"`
	}
}

const (
	configIDPrefix = "claude:apikey:"
	sourcePrefix   = "config:claude["
)

// inScope reports whether c is a Claude API key from CPA's configuration:
// provider claude, auth kind apikey, an ID of the config-key shape, and the
// source attribute CPA's synthesizer writes for exactly that ID,
// "config:claude[<the ID's hash and suffix>]". It compares by slicing, so it
// allocates nothing.
func (c *pickCandidate) inScope() bool {
	if c.Provider != "claude" || c.Attributes.AuthKind != "apikey" || !qc.ConfigAPIKeyAuthID(c.ID) {
		return false
	}
	src, token := c.Attributes.Source, c.ID[len(configIDPrefix):]
	return len(src) == len(sourcePrefix)+len(token)+1 &&
		src[:len(sourcePrefix)] == sourcePrefix &&
		src[len(sourcePrefix):len(src)-1] == token &&
		src[len(src)-1] == ']'
}

// configID keeps a value only when it has CPA's config API-key auth ID shape
// (qc.ConfigAPIKeyAuthID). The raw JSON is tested first: a plain string with no
// `\` anywhere, 26 to 31 bytes between the quotes, starting with
// "claude:apikey:". Anything else, including an OAuth file name or an e-mail
// from a higher-tier previous attempt, is never turned into a string. It never
// fails the decode.
type configID string

var quotedConfigIDPrefix = []byte(`"` + configIDPrefix)

func (c *configID) UnmarshalJSON(b []byte) error {
	*c = ""
	if n := len(b) - 2; n < 26 || n > 31 || b[len(b)-1] != '"' ||
		!bytes.HasPrefix(b, quotedConfigIDPrefix) || bytes.IndexByte(b, '\\') >= 0 {
		return nil
	}
	if id := string(b[1 : len(b)-1]); qc.ConfigAPIKeyAuthID(id) {
		*c = configID(id)
	}
	return nil
}

// validIndex reports whether s is a config index as CPA writes one: 1 to 5
// ASCII digits.
func validIndex(s string) bool {
	if len(s) < 1 || len(s) > 5 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
