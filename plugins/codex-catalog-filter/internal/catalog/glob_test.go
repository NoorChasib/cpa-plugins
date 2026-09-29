package catalog

import "testing"

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern string
		match   []string
		reject  []string
	}{
		{"gpt-[0-9]*", []string{"gpt-6-astra", "gpt-5.6-terra", "gpt-5.5", "gpt-7"}, []string{"gpt-image-2", "gpt-reserve", "gpt-", "xgpt-6", "GPT-6-sol"}},
		{"codex-*", []string{"codex-auto-review", "codex-"}, []string{"codex", "or-codex-mini"}},
		{"*", []string{"", "anything/with/slashes"}, nil},
		{"or-*", []string{"or-kimi-k2", "or-"}, []string{"cpa-or-x", "ORx"}},
		{"*/gpt-*", []string{"team/gpt-6-sol", "a/b/gpt-x"}, []string{"gpt-6-sol"}},
		{"gpt-?.?", []string{"gpt-5.5", "gpt-6.1"}, []string{"gpt-5.55", "gpt-5"}},
		{"gpt-[!0-9]*", []string{"gpt-image-2", "gpt-reserve"}, []string{"gpt-6-sol"}},
		{"gpt-[^0-9]*", []string{"gpt-image-2"}, []string{"gpt-5.5"}},
		{"model-[]x]", []string{"model-]", "model-x"}, []string{"model-y"}},
		{"a[-z]", []string{"a-", "az"}, []string{"ab"}},
		{"a[z-]", []string{"a-", "az"}, []string{"ab"}},
		{`literal\*`, []string{"literal*"}, []string{"literalX"}},
		{"dots.and+plus(1)", []string{"dots.and+plus(1)"}, []string{"dotsXand+plus(1)"}},
		{"***x", []string{"x", "abcx"}, []string{"abc"}},
		{"é-*", []string{"é-model"}, []string{"e-model"}},
		{"gpt-*", []string{"gpt-a\nb"}, []string{"gpt\n-a"}},
		{"gpt-?", []string{"gpt-\n"}, nil},
	}
	for _, tc := range cases {
		re, err := compileGlob(tc.pattern)
		if err != nil {
			t.Fatalf("%q: %v", tc.pattern, err)
		}
		for _, s := range tc.match {
			if !re.MatchString(s) {
				t.Errorf("%q should match %q", tc.pattern, s)
			}
		}
		for _, s := range tc.reject {
			if re.MatchString(s) {
				t.Errorf("%q should not match %q", tc.pattern, s)
			}
		}
	}
}

func TestGlobRejectsMalformedPatterns(t *testing.T) {
	for _, pattern := range []string{"gpt-[0-9", "[", "[]", "[!]", `trailing\`, "[z-a]", `[a\`} {
		if _, err := compileGlob(pattern); err == nil {
			t.Errorf("%q should be rejected", pattern)
		}
	}
}
