package mask

// ruleSpec is one masking rule as data. rules_gitleaks_v8_30_1.go (generated) and rules_ca.go are
// lists of these; compile.go turns them into the engine's compiled form.
type ruleSpec struct {
	ID          string
	Regex       string   // RE2 syntax
	Keywords    []string // lowercase; the rule runs only in windows around a keyword hit
	Entropy     float64  // 0: no gate; else the secret's Shannon entropy must be strictly greater
	SecretGroup int      // 0: first non-empty capture group, else the whole match
	Allow       []allowSpec

	// gate is an extra explicit check on the secret (ca-* rules only).
	gate func(secret string) bool
}

// allowSpec is one gitleaks allowlist without path and commit checks.
type allowSpec struct {
	RegexTarget string // "" (the secret), "match" or "line"
	Regexes     []string
	StopWords   []string // lowercase; matched as substrings of the lowercased secret
	Condition   string   // "" or "OR": any check; "AND": every present check
}
