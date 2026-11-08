package mask

import (
	"strings"
)

// caRules are the 18 explicit chat-archive rules, in a fixed order; ca-archive-token matches only
// source tokens and enrollment codes (cha_src_, cha_enr_). They run before the gitleaks rules: on overlapping spans the longest secret
// wins and ties go to the earlier rule, so this order matters.
var caRules = []ruleSpec{
	{ID: "ca-anthropic", Regex: `\b(sk-ant-[a-z]{2,8}[0-9]{2}-[A-Za-z0-9_-]{20,})`, Keywords: []string{"sk-ant-"}},
	{ID: "ca-sk", Regex: `\b(sk-[A-Za-z0-9][A-Za-z0-9_-]{19,})`, Keywords: []string{"sk-"}, Entropy: 3.0, gate: hasDigit},
	{ID: "ca-stripe", Regex: `\b((?:sk|rk)_(?:live|test|prod)_[A-Za-z0-9]{10,})`, Keywords: []string{"sk_live_", "rk_live_", "sk_test_", "rk_test_", "sk_prod_", "rk_prod_"}},
	{ID: "ca-xai", Regex: `\b(xai-[A-Za-z0-9_-]{20,})`, Keywords: []string{"xai-"}},
	{ID: "ca-google-api-key", Regex: `\b(AIza[0-9A-Za-z_-]{35})`, Keywords: []string{"aiza"}},
	{ID: "ca-google-oauth", Regex: `\b(ya29\.[0-9A-Za-z_-]{20,}|1//0[0-9A-Za-z_-]{30,})`, Keywords: []string{"ya29.", "1//0"}},
	{ID: "ca-aws-secret", Regex: `(?i)\b(?:aws_?)?secret_?(?:access_?)?key(?:_?id)?["'\s]{0,3}[:=]{1,2}\s{0,3}["']?([A-Za-z0-9/+]{40})(?:[^A-Za-z0-9/+]|$)`, Keywords: []string{"secret"}},
	{ID: "ca-github", Regex: `\b((?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})`, Keywords: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}},
	{ID: "ca-gitlab", Regex: `\b(glpat-[A-Za-z0-9_.-]{20,})`, Keywords: []string{"glpat-"}},
	{ID: "ca-hf", Regex: `\b(hf_[A-Za-z0-9]{30,})`, Keywords: []string{"hf_"}},
	{ID: "ca-npm", Regex: `\b(npm_[A-Za-z0-9]{36})`, Keywords: []string{"npm_"}},
	{ID: "ca-slack", Regex: `\b(xox[abposre]-[A-Za-z0-9-]{10,}|xapp-[0-9]-[A-Za-z0-9-]{10,})`, Keywords: []string{"xox", "xapp-"}},
	{ID: "ca-jwt", Regex: `\b(eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`, Keywords: []string{"eyj"}},
	{ID: "ca-auth-header", Regex: `(?i)\b(?:proxy-)?authorization["']?\s*[:=]\s*["']?(?:bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{12,})`, Keywords: []string{"authorization"}},
	{ID: "ca-api-key-header", Regex: `(?i)\b(?:x-api-key|api-key|x-goog-api-key)["']?\s*[:=]\s*["']?([A-Za-z0-9._~+/=-]{16,})`, Keywords: []string{"api-key"}},
	{ID: "ca-url-cred", Regex: `(?i)[?&;](?:key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|token|auth|password|passwd|sig|signature|x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature|x-goog-credential)=([^&\s"'#<>\\]{8,})`, Keywords: []string{"key=", "token=", "auth=", "sig=", "signature=", "password=", "passwd=", "x-amz-", "x-goog-"}},
	{ID: "ca-url-userinfo", Regex: `(?i)\b[a-z][a-z0-9+.-]{1,20}://[^\s:@/]{1,64}:([^\s:@/]{6,128})@`, Keywords: []string{"://"}},
	{ID: "ca-archive-token", Regex: `\b(cha_(?:src|enr)_[A-Za-z0-9]{32,})`, Keywords: []string{"cha_"}},
}

// hasDigit is ca-sk's gate: at least one ASCII digit in the secret.
func hasDigit(secret string) bool {
	return strings.ContainsAny(secret, "0123456789")
}

// wholeMatchRules are the gitleaks rules whose secret is the whole match instead of gitleaks' first
// non-empty capture group. For both, that group is a short fixed-shape fragment: about 10 characters
// of a base64-encoded JWT's header (jwt-base64), and 5 characters of the first GUID of a Microsoft
// Teams webhook URL (microsoft-teams-webhook). Masking only the group would leave the encoded payload
// and signature, or the webhook's 32-character secret, readable. Entropy, allowlists, the tag and
// Detect's Span all apply to the whole match. TestWeakFirstGroupRules checks that these are exactly
// the rules whose group carries far less randomness than the match. Changing this list changes mask/v1
// output, but not RulesetSHA256, which covers only the generated file.
var wholeMatchRules = map[string]bool{
	"jwt-base64":              true,
	"microsoft-teams-webhook": true,
}

// window is a per-rule scan window around a keyword hit: [hitStart-before, hitEnd+after], not
// limited to the line. Rules not listed use the default: the line containing the
// hit, at most 1 KiB either side.
type window struct{ before, after int }

const defaultLineRadius = 1024

var ruleWindows = map[string]window{
	"generic-api-key":  {before: 64, after: 260},
	"jwt":              {before: 1, after: 2048},
	"ca-url-userinfo":  {before: 24, after: 200},
	"private-key":      {before: 512, after: 16 << 10},
	"curl-auth-header": {before: 512, after: 16 << 10},
}

// taggedRules is the reviewed tag list: rules whose placeholder carries tag12. It holds the ca-* rules
// for vendor tokens (every one but the header and URL rules), four gitleaks rules reviewed by name
// (aws-access-token, private-key, jwt, jwt-base64), plus the gitleaks vendor-token rules whose secret
// group starts with a fixed vendor prefix and carries at least 80 bits of randomness
// (TestTaggedRulesReviewed checks both properties mechanically). Never on the list: generic-api-key,
// curl-auth-*, header, URL and password-like rules.
var taggedRules = map[string]bool{
	// ca-* vendor-token rules.
	"ca-anthropic":      true,
	"ca-sk":             true,
	"ca-stripe":         true,
	"ca-xai":            true,
	"ca-google-api-key": true,
	"ca-google-oauth":   true,
	"ca-aws-secret":     true,
	"ca-github":         true,
	"ca-gitlab":         true,
	"ca-hf":             true,
	"ca-npm":            true,
	"ca-slack":          true,
	"ca-jwt":            true,
	"ca-archive-token":  true,

	// gitleaks rules reviewed by name.
	"aws-access-token": true,
	"private-key":      true,
	"jwt":              true,
	"jwt-base64":       true,

	// gitleaks vendor-token rules: secret starts with a fixed vendor prefix of at least 2 characters
	// and has at least 80 bits of randomness. Reviewed out: planetscale-password (password-like).
	"1password-secret-key":                        true,
	"1password-service-account-token":             true,
	"adobe-client-secret":                         true,
	"age-secret-key":                              true,
	"airtable-personnal-access-token":             true,
	"alibaba-access-key-id":                       true,
	"anthropic-admin-api-key":                     true,
	"anthropic-api-key":                           true,
	"artifactory-api-key":                         true,
	"artifactory-reference-token":                 true,
	"authress-service-client-access-key":          true,
	"aws-amazon-bedrock-api-key-long-lived":       true,
	"beamer-api-token":                            true,
	"clickhouse-cloud-api-secret-key":             true,
	"clojars-api-token":                           true,
	"cloudflare-origin-ca-key":                    true,
	"databricks-api-token":                        true,
	"defined-networking-api-token":                true,
	"digitalocean-access-token":                   true,
	"digitalocean-pat":                            true,
	"digitalocean-refresh-token":                  true,
	"doppler-api-token":                           true,
	"dropbox-short-lived-api-token":               true,
	"duffel-api-token":                            true,
	"dynatrace-api-token":                         true,
	"easypost-api-token":                          true,
	"easypost-test-api-token":                     true,
	"facebook-page-access-token":                  true,
	"flutterwave-public-key":                      true,
	"flutterwave-secret-key":                      true,
	"flyio-access-token":                          true,
	"frameio-api-token":                           true,
	"gcp-api-key":                                 true,
	"github-app-token":                            true,
	"github-fine-grained-pat":                     true,
	"github-oauth":                                true,
	"github-pat":                                  true,
	"github-refresh-token":                        true,
	"gitlab-cicd-job-token":                       true,
	"gitlab-deploy-token":                         true,
	"gitlab-feature-flag-client-token":            true,
	"gitlab-feed-token":                           true,
	"gitlab-incoming-mail-token":                  true,
	"gitlab-kubernetes-agent-token":               true,
	"gitlab-oauth-app-secret":                     true,
	"gitlab-pat":                                  true,
	"gitlab-pat-routable":                         true,
	"gitlab-ptt":                                  true,
	"gitlab-rrt":                                  true,
	"gitlab-runner-authentication-token":          true,
	"gitlab-runner-authentication-token-routable": true,
	"gitlab-scim-token":                           true,
	"gitlab-session-cookie":                       true,
	"gocardless-api-token":                        true,
	"grafana-api-key":                             true,
	"grafana-cloud-api-token":                     true,
	"grafana-service-account-token":               true,
	"harness-api-key":                             true,
	"heroku-api-key-v2":                           true,
	"huggingface-access-token":                    true,
	"huggingface-organization-api-token":          true,
	"infracost-api-token":                         true,
	"intra42-client-secret":                       true,
	"linear-api-key":                              true,
	"lob-api-key":                                 true,
	"lob-pub-api-key":                             true,
	"mailgun-private-api-token":                   true,
	"mailgun-pub-key":                             true,
	"mapbox-api-token":                            true,
	"new-relic-browser-api-token":                 true,
	"new-relic-insert-key":                        true,
	"new-relic-user-api-key":                      true,
	"notion-api-token":                            true,
	"npm-access-token":                            true,
	"octopus-deploy-api-key":                      true,
	"okta-access-token":                           true,
	"openai-api-key":                              true,
	"openshift-user-token":                        true,
	"perplexity-api-key":                          true,
	"plaid-api-token":                             true,
	"planetscale-api-token":                       true,
	"planetscale-oauth-token":                     true,
	"postman-api-token":                           true,
	"prefect-api-token":                           true,
	"pulumi-api-token":                            true,
	"pypi-upload-token":                           true,
	"readme-api-token":                            true,
	"rubygems-api-token":                          true,
	"scalingo-api-token":                          true,
	"sendgrid-api-token":                          true,
	"sendinblue-api-token":                        true,
	"sentry-org-token":                            true,
	"sentry-user-token":                           true,
	"settlemint-application-access-token":         true,
	"settlemint-personal-access-token":            true,
	"settlemint-service-access-token":             true,
	"shippo-api-token":                            true,
	"shopify-access-token":                        true,
	"shopify-custom-access-token":                 true,
	"shopify-private-app-access-token":            true,
	"shopify-shared-secret":                       true,
	"slack-config-access-token":                   true,
	"slack-config-refresh-token":                  true,
	"slack-legacy-bot-token":                      true,
	"slack-user-token":                            true,
	"square-access-token":                         true,
	"twilio-api-key":                              true,
	"twitter-bearer-token":                        true,
	"typeform-api-token":                          true,
	"vault-batch-token":                           true,
	"vault-service-token":                         true,
	"yandex-access-token":                         true,
	"yandex-api-key":                              true,
	"yandex-aws-access-token":                     true,
}
