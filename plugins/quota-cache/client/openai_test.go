package client

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

// An OpenAI organization is read as the openai-organization header names it:
// trimmed, its case kept, in the shape captured values have. Anything a
// header or the page could carry that is not that shape is refused, whole.
func TestNormalizeOpenAIOrganization(t *testing.T) {
	longest := "acme-" + strings.Repeat("x", 59)
	for _, tc := range []struct{ in, want string }{
		{"user-AbCdEfGhIjKlMnOpQrStUvWx", "user-AbCdEfGhIjKlMnOpQrStUvWx"},
		{"acme-x1wegt", "acme-x1wegt"},
		{"  acme-x1wegt\n", "acme-x1wegt"},
		{"\tUser-AbCdEfGhIjKlMnOpQrStUvWx ", "User-AbCdEfGhIjKlMnOpQrStUvWx"},
		{"ACME-X1WEGT", "ACME-X1WEGT"},
		{"acme.corp_eu-1", "acme.corp_eu-1"},
		{"a", "a"},
		{"0", "0"},
		{longest, longest},
		// An Organization ID has the shape too. OpenAI's responses do not name
		// an organization that way today; the page warns about it instead.
		{"org-AbCdEfGhIjKlMnOpQrStUvWx", "org-AbCdEfGhIjKlMnOpQrStUvWx"},
	} {
		got, ok := NormalizeOpenAIOrganization(tc.in)
		if !ok || got != tc.want {
			t.Errorf("NormalizeOpenAIOrganization(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
			continue
		}
		// What it returns is what it keeps: a stored slug passes unchanged.
		if again, ok := NormalizeOpenAIOrganization(got); !ok || again != got {
			t.Errorf("NormalizeOpenAIOrganization(%q) = %q, %v; not stable", got, again, ok)
		}
	}
	if len(longest) != 64 {
		t.Fatalf("the longest slug is %d bytes, want 64", len(longest))
	}
	for _, bad := range []string{
		"", "   ", "\n",
		longest + "x",
		"-acme",
		".acme",
		"_acme",
		"acme corp",
		"acme\tcorp",
		"acme\ncorp",
		"acme\x00",
		"acmé-x1wegt",
		"ａcme-x1wegt",
		"acme/x1wegt",
		"acme:x1wegt",
		"acme+x1wegt",
		`acme"x1wegt`,
		"acme,acme-x1wegt",
	} {
		if got, ok := NormalizeOpenAIOrganization(bad); ok || got != "" {
			t.Errorf("NormalizeOpenAIOrganization(%q) = %q, %v; want refused", bad, got, ok)
		}
	}
}

// An OpenAI organization's account is the hash of its slug, case kept, so
// two slugs that differ only in case are two accounts, as the header names
// two organizations. Its prefix tells it from a Claude account and from an
// OpenAI Organization ID, and the slug cannot be read back from it.
func TestOpenAICreditOrgAccount(t *testing.T) {
	if OpenAICreditAccountPrefix != "oai-" {
		t.Fatalf("OpenAICreditAccountPrefix = %q", OpenAICreditAccountPrefix)
	}
	for slug, want := range map[string]string{
		"user-AbCdEfGhIjKlMnOpQrStUvWx": "oai-dd79fc440924",
		"User-AbCdEfGhIjKlMnOpQrStUvWx": "oai-497a0574eff6",
		"acme-x1wegt":                   "oai-ed5b51206e7d",
	} {
		if got := OpenAICreditOrgAccount(slug); got != want {
			t.Errorf("OpenAICreditOrgAccount(%q) = %q, want %q", slug, got, want)
		}
		if got := OpenAICreditOrgAccount(" " + slug + "\n"); got != want {
			t.Errorf("spacing around %q changed its account to %q", slug, got)
		}
		account := OpenAICreditOrgAccount(slug)
		if !regexp.MustCompile(`^oai-[0-9a-f]{12}$`).MatchString(account) {
			t.Errorf("account = %q", account)
		}
		for _, part := range strings.Split(slug, "-") {
			if len(part) >= 6 && strings.Contains(account, part) {
				t.Errorf("%q leaked into the account id %q", part, account)
			}
		}
	}
	if a := OpenAICreditOrgAccount("acme-x1wegt"); a == OpenAICreditOrgAccount("ACME-X1WEGT") {
		t.Fatal("two slugs that differ in case share an account")
	}
	// The prefix alone keeps the two providers' accounts apart, and keeps an
	// OpenAI account from reading as an org-… Organization ID.
	if a := OpenAICreditOrgAccount("org-AbCdEfGhIjKlMnOpQrStUvWx"); strings.HasPrefix(a, "org-") || a == APICreditOrgAccount("org-AbCdEfGhIjKlMnOpQrStUvWx") {
		t.Fatalf("account = %q", a)
	}
}

// The four OpenAI files sit beside the snapshot under its name, as the Claude
// files do.
func TestOpenAIPaths(t *testing.T) {
	for in, want := range map[string][4]string{
		DefaultPath: {
			"plugins/data/quota-cache/snapshot.openai-meter.json", "plugins/data/quota-cache/snapshot.openai-credits.json",
			"plugins/data/quota-cache/snapshot.openai-organizations.json", "plugins/data/quota-cache/snapshot.openai-settings.json",
		},
		"/data/cache.json": {"/data/cache.openai-meter.json", "/data/cache.openai-credits.json", "/data/cache.openai-organizations.json", "/data/cache.openai-settings.json"},
		"/data/snapshot":   {"/data/snapshot.openai-meter.json", "/data/snapshot.openai-credits.json", "/data/snapshot.openai-organizations.json", "/data/snapshot.openai-settings.json"},
	} {
		for i, path := range []func(string) string{OpenAIMeterPath, OpenAICreditsPath, OpenAIOrganizationsPath, OpenAISettingsPath} {
			if got := path(in); got != want[i] {
				t.Errorf("path %d of %q = %q, want %q", i, in, got, want[i])
			}
		}
		claude := map[string]bool{MeterPath(in): true, CreditsPath(in): true, OrganizationsPath(in): true}
		for _, path := range want {
			if claude[path] {
				t.Errorf("%q is a Claude file's path too", path)
			}
		}
	}
}

// Every service_tier OpenAI documents for a response maps to the tier it is
// priced at, and anything else to one that is never priced.
func TestNormalizeOpenAIServiceTier(t *testing.T) {
	for in, want := range map[string]string{
		"":            "",
		"auto":        "",
		"default":     "",
		"scale":       "",
		"flex":        MeterTierFlex,
		"priority":    MeterTierFast,
		"fast":        MeterTierFast,
		"ultrafast":   MeterTierUltrafast,
		" Priority ":  MeterTierFast,
		"FLEX":        MeterTierFlex,
		"Default":     "",
		"\tUltraFast": MeterTierUltrafast,
		"  ":          "",
		"turbo":       MeterTierUnknown,
		"batch":       MeterTierUnknown,
		"standard":    MeterTierUnknown,
		"ultra fast":  MeterTierUnknown,
		"flex2":       MeterTierUnknown,
	} {
		if got := NormalizeOpenAIServiceTier(in); got != want {
			t.Errorf("NormalizeOpenAIServiceTier(%q) = %q, want %q", in, got, want)
		}
	}
	for got, want := range map[string]string{
		MeterTierFlex:      "flex",
		MeterTierFast:      "fast",
		MeterTierUltrafast: "ultrafast",
		MeterTierUnknown:   "unknown",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
		// A stored tier reads back as itself.
		if again := NormalizeOpenAIServiceTier(got); again != got {
			t.Errorf("NormalizeOpenAIServiceTier(%q) = %q, not stable", got, again)
		}
	}
}

// Long context is over 272,000 input tokens, not 272×1024, and the class is
// the same whatever the model.
func TestOpenAIMeterPromptClass(t *testing.T) {
	if MeterPromptOver272K != "over_272k" {
		t.Fatalf("MeterPromptOver272K = %q", MeterPromptOver272K)
	}
	for _, tc := range []struct {
		tokens uint64
		want   string
	}{
		{0, ""},
		{272000, ""},
		{272001, MeterPromptOver272K},
		{272 * 1024, MeterPromptOver272K},
		{math.MaxUint64, MeterPromptOver272K},
	} {
		if got := OpenAIMeterPromptClass(tc.tokens); got != tc.want {
			t.Errorf("OpenAIMeterPromptClass(%d) = %q, want %q", tc.tokens, got, tc.want)
		}
	}
	// Claude's class never becomes OpenAI's.
	if got := MeterPromptClass("gpt-6-sol", 900000); got != "" {
		t.Errorf("MeterPromptClass of an OpenAI model = %q", got)
	}
}
