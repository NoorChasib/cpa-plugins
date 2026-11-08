package mask

// Masking benchmarks on synthetic dense text. Nothing is gated here; the real-text gate is the
// importer's mask-dry-run. Run:
//
//	go test -run '^$' -bench . -benchtime 1x ./pkg/mask/
//
// BenchmarkPerRule reports MB/s for every rule alone (its windows' regex and checks);
// BenchmarkExactSubRuns reports the exact-value sub-run tests alone; BenchmarkPrefilter the shared
// lowercase copy and Aho-Corasick pass.

import (
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
)

var (
	denseOnce  sync.Once
	denseSmall string // 1 MiB, for per-rule runs
	denseLarge string // 8.5 MB, a large cold body
)

func denseTexts() (string, string) {
	denseOnce.Do(func() {
		denseSmall = denseText(1<<20, 1)
		denseLarge = denseText(8_500_000, 2)
	})
	return denseSmall, denseLarge
}

// denseText builds synthetic text deliberately dense in rule keywords (about one word in seven):
// random words, code-like assignment lines, URLs, hashes, and one planted fake secret per 1,000 lines.
func denseText(size int, seed uint64) string {
	r := rand.New(rand.NewPCG(seed, 99))
	words := []string{"the", "of", "request", "model", "response", "file", "function", "value", "return", "error",
		"session", "user", "assistant", "content", "message", "stream", "index", "config", "build", "test",
		"deploy", "server", "client", "update", "branch", "commit", "review", "result", "output", "input"}
	keywords := []string{"token", "key", "api", "auth", "secret", "password", "access", "credential", "bearer", "sk-"}
	rs := func(alpha string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.IntN(len(alpha))]
		}
		return string(b)
	}
	const an = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	plants := []func() string{
		func() string { return "ghp_" + rs(an, 36) },
		func() string { return "sk-ant-api03-" + rs(an, 93) + "AA" },
		func() string { return "AKIA" + rs("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16) },
		func() string { return "xoxb-" + rs("0123456789", 12) + "-" + rs(an, 24) },
		func() string { return `password = "` + rs(an, 24) + `"` },
	}
	var b strings.Builder
	b.Grow(size + 256)
	for line := 1; b.Len() < size; line++ {
		switch {
		case line%1000 == 0:
			b.WriteString("planted " + plants[r.IntN(len(plants))]() + " here")
		case line%7 == 0:
			b.WriteString(words[r.IntN(len(words))] + "_" + keywords[r.IntN(len(keywords))] + " = \"" + rs(an, 8+r.IntN(12)) + "\"")
		case line%11 == 0:
			b.WriteString("see https://docs.example.invalid/" + words[r.IntN(len(words))] + "/" + rs(an, 10) + "?page=" + rs("0123456789", 2))
		case line%13 == 0:
			b.WriteString("sha256 " + rs("0123456789abcdef", 64))
		default:
			for w := 8 + r.IntN(8); w > 0; w-- {
				if r.IntN(7) == 0 {
					b.WriteString(keywords[r.IntN(len(keywords))])
				} else {
					b.WriteString(words[r.IntN(len(words))])
				}
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func benchExactConfig() Config {
	var cfg Config
	for i := 0; i < 16; i++ {
		v := strings.Repeat(string(rune('a'+i)), 8) + "0123456789ABCDEFGHIJ"
		cfg.ExactFPs = append(cfg.ExactFPs, prefixedSum(exactFPPrefix, v))
		cfg.CallerFPs = append(cfg.CallerFPs, prefixedSum(callerScopePrefix, v))
	}
	return cfg
}

func benchString(b *testing.B, cfg Config, text string) {
	m, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.String(text)
	}
}

// BenchmarkMaskDense is String with all 235 rules and no exact set on 8.5 MB. The text holds planted
// secrets, so it takes two passes (the second finds nothing).
func BenchmarkMaskDense(b *testing.B) {
	_, text := denseTexts()
	benchString(b, Config{}, text)
}

// BenchmarkMaskDenseWithExact adds a 16 + 16 fingerprint exact set, so every run of 20-512 bytes
// gets its sub-run tests.
func BenchmarkMaskDenseWithExact(b *testing.B) {
	_, text := denseTexts()
	benchString(b, benchExactConfig(), text)
}

// BenchmarkScanOnePass is one scan (rules and exact values) of the same text.
func BenchmarkScanOnePass(b *testing.B) {
	_, text := denseTexts()
	m, err := New(benchExactConfig())
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.scan(text)
	}
}

// BenchmarkExactSubRuns is the exact-value pass alone: runs of 20-512 bytes, up to 10 sub-candidate
// tests each, ExactFP and CallerScope per test.
func BenchmarkExactSubRuns(b *testing.B) {
	_, text := denseTexts()
	m, err := New(benchExactConfig())
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.addExact(text, nil)
	}
}

// BenchmarkPrefilter is the lowercase copy, the Aho-Corasick walk and window merging.
func BenchmarkPrefilter(b *testing.B) {
	_, text := denseTexts()
	rs, err := rulesetCompiled()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rs.windows(text)
	}
}

// BenchmarkPerRule reports MB/s for each rule alone on 1 MiB of dense text: its windows' regex and
// checks, given the shared prefilter.
func BenchmarkPerRule(b *testing.B) {
	text, _ := denseTexts()
	rs, err := rulesetCompiled()
	if err != nil {
		b.Fatal(err)
	}
	all := rs.windows(text)
	for ri := range rs.rules {
		var wins []scanWin
		for _, w := range all {
			if w.rule == ri {
				wins = append(wins, w)
			}
		}
		b.Run(rs.rules[ri].id, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for i := 0; i < b.N; i++ {
				rs.candidates(text, wins)
			}
		})
	}
}
