package config

import "testing"

func TestQuotaCacheOptIn(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"standalone default", "", ""},
		{"toggle on", "use-quota-cache: true\n", "plugins/data/quota-cache/snapshot.json"},
		{"custom path", "use-quota-cache: true\nquota-cache-path: /data/quota.json\n", "/data/quota.json"},
		{"toggle off overrides path", "use-quota-cache: false\nquota-cache-path: /data/quota.json\n", ""},
		{"legacy opt in", "quota-cache-path: /data/quota.json\n", "/data/quota.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.QuotaCachePath != tc.want {
				t.Fatalf("effective cache path = %q, want %q", cfg.QuotaCachePath, tc.want)
			}
		})
	}
}
