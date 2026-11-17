package mask

// GenericStopword reports whether one of generic-api-key's allowlist stopwords occurs in secret. Test
// helper only (compiled with the tests): randomly generated generic canaries redraw values that
// gitleaks would allowlist, so a canary failure always means a masking bug.
func GenericStopword(secret string) bool {
	rs, err := rulesetCompiled()
	if err != nil {
		panic(err)
	}
	for _, r := range rs.rules {
		if r.id != "generic-api-key" {
			continue
		}
		for _, al := range r.allow {
			if al.stops != nil && al.stops.MatchFirst(asciiLowerUnicode(secret)) != nil {
				return true
			}
		}
	}
	return false
}

// GenericSkipped reports whether generic-api-key skips secret by design: one of its allowlist stopwords
// occurs in it, or its Shannon entropy is not above the rule's entropy gate. Test helper only: generic
// canaries redraw such values (a few percent of short composed canaries fall under the gate), so a canary
// failure always means a masking bug.
func GenericSkipped(secret string) bool {
	if GenericStopword(secret) {
		return true
	}
	rs, err := rulesetCompiled()
	if err != nil {
		panic(err)
	}
	for _, r := range rs.rules {
		if r.id == "generic-api-key" && r.entropy != 0 && shannonEntropy(secret) <= r.entropy {
			return true
		}
	}
	return false
}
