package importer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/importer/synth"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/mask"
	"github.com/NoorChasib/cpa-plugins/plugins/chat-archive/pkg/wire"
)

// Gate budgets of the real-corpus masking gate: one cold input of the 8.5 MB class in at most 3 s and of
// the 50 MB class in at most 20 s, per core.
const (
	gateSmallBytes = 8_500_000
	gateSmallLimit = 3 * time.Second
	gateLargeBytes = 50_000_000
	gateLargeLimit = 20 * time.Second
)

type dryRunOpts struct {
	root        string
	sampleBytes int64
	synthMB     int
	seed        uint64
}

// dryInput is one input of mask-dry-run: a whole file, or the synthetic text.
type dryInput struct {
	ref   fileRef
	synth string
	size  int64
	dur   time.Duration // thread CPU time when the platform has it, else wall time
	wall  time.Duration
	err   bool    // the build failed (counted, never printed: an error text could quote input)
	top   string  // the rule (or prefilter/exact) with the most profiled time in this input
	share float64 // its share of the input's profiled time
}

// maskDryRun masks every input through the importer's own record and file builders, without
// uploading, and prints matches per rule and per GB, MB/s per rule and cold times per input class:
// counts, sizes, rule ids and timings only, never content or paths. Inputs are processed one at a time
// on one goroutine, each with a fresh memo (cold).
func (e *Env) maskDryRun(o dryRunOpts) error {
	p := pathsFor(e.Home)
	cfg, err := loadConfig(p)
	if err != nil && !errors.Is(err, errNotEnrolled) {
		return err
	}
	roots := cfg.Roots
	if len(roots) == 0 {
		roots = DefaultRoots(e.Home)
	}
	if roots, err = selectRoots(roots, o.root); err != nil {
		return err
	}
	files, err := discover(roots)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if o.sampleBytes > 0 {
		files = stratifiedSample(files, o.sampleBytes, o.seed)
	}
	var inputs []*dryInput
	for _, f := range files {
		inputs = append(inputs, &dryInput{ref: f, size: f.Size})
	}
	if o.synthMB > 0 {
		inputs = append(inputs, &dryInput{synth: synth.Text(o.synthMB*1_000_000, o.seed), size: int64(o.synthMB) * 1_000_000})
	}
	m, err := mask.New(mask.Config{ExactFPs: [][32]byte{{0}}}) // an exact set makes the exact-value pass run, as in a real run
	if err != nil {
		return err
	}
	stats := newRunStats()
	ruleTime := map[string]time.Duration{}
	var profBytes int64
	var profWall, profCPU time.Duration // the profiling pass, to turn its wall-clock rule times into CPU time
	var sampled, synthBytes int64
	runtime.LockOSThread() // thread CPU time measures this goroutine's core only
	defer runtime.UnlockOSThread()
	_, cpuOK := threadCPU()
	start := time.Now()
	var cpuTotal time.Duration
	failed := 0
	for _, in := range inputs {
		b := &builder{ctx: context.Background(), m: m, memo: newMemo(), limits: wire.DefaultLimits, stats: stats}
		c0, _ := threadCPU()
		t0 := time.Now()
		if err := dryInputRun(b, in); err != nil {
			in.err = true
			failed++
		}
		in.wall = time.Since(t0)
		in.dur = in.wall
		if cpuOK {
			c1, _ := threadCPU()
			in.dur = c1 - c0
		}
		cpuTotal += in.dur
		sampled += in.size
		if in.synth != "" {
			synthBytes += in.size
		}
		acc := map[string]time.Duration{}
		pc0, _ := threadCPU()
		pw0 := time.Now()
		profBytes += profileInput(m, in, acc)
		profWall += time.Since(pw0)
		if pc1, ok := threadCPU(); ok {
			profCPU += pc1 - pc0
		}
		var sum time.Duration
		for id, d := range acc {
			ruleTime[id] += d
			sum += d
			if d > acc[in.top] || in.top == "" {
				in.top = id
			}
		}
		if sum > 0 {
			in.share = float64(acc[in.top]) / float64(sum)
		}
	}
	wall := time.Since(start)

	w := e.Stdout
	clock := "wall"
	if cpuOK {
		clock = "thread CPU"
	}
	fmt.Fprintf(w, "mask-dry-run: %d inputs, %.3f GB (%.3f GB synthetic) of %.3f GB discovered; wall %.1fs, %s %.1fs, %.2f MB/s per core\n",
		len(inputs), gb(sampled), gb(synthBytes), gb(total), wall.Seconds(), clock, cpuTotal.Seconds(), mbps(sampled, cpuTotal))
	if failed > 0 {
		fmt.Fprintf(w, "inputs whose build failed (timed up to the failure, excluded from the gate): %d\n", failed)
	}
	stats.mu.Lock()
	rules := make([]string, 0, len(stats.matches))
	for r := range stats.matches {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		if stats.matches[rules[i]] != stats.matches[rules[j]] {
			return stats.matches[rules[i]] > stats.matches[rules[j]]
		}
		return rules[i] < rules[j]
	})
	fmt.Fprintf(w, "matches per rule (count, per GB of input):\n")
	for _, r := range rules {
		fmt.Fprintf(w, "  %-40s %10d %12.1f/GB\n", r, stats.matches[r], float64(stats.matches[r])/math.Max(gb(sampled), 1e-9))
	}
	if len(rules) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	fmt.Fprintf(w, "masked string bytes: %.3f GB\n", gb(stats.maskBytes))
	stats.mu.Unlock()
	type rt struct {
		id string
		d  time.Duration
	}
	var rs []rt
	for id, d := range ruleTime {
		rs = append(rs, rt{id, d})
	}
	// Rule times are wall clock inside the profiling pass; on a loaded machine they are scaled by
	// that pass's thread CPU / wall ratio, so they read as per-core time.
	scale := 1.0
	if cpuOK && profWall > 0 && profCPU > 0 {
		scale = float64(profCPU) / float64(profWall)
	}
	for i := range rs {
		rs[i].d = time.Duration(float64(rs[i].d) * scale)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].d > rs[j].d })
	fmt.Fprintf(w, "MB/s per rule over %.3f GB of string leaves, per core (CPU/wall scale %.2f; slowest first; time share):\n", gb(profBytes), scale)
	var sum time.Duration
	for _, x := range rs {
		sum += x.d
	}
	for _, x := range rs {
		fmt.Fprintf(w, "  %-40s %12.1f MB/s %6.2f%% %8.2fs\n", x.id, mbps(profBytes, x.d), 100*x.d.Seconds()/math.Max(sum.Seconds(), 1e-9), x.d.Seconds())
	}
	small, large := gateInputs(inputs)
	pass := true
	report := func(name string, in *dryInput, limit time.Duration, target int64) {
		if in == nil {
			fmt.Fprintf(w, "cold %s: no input of this class\n", name)
			pass = false
			return
		}
		kind := "real"
		if in.synth != "" {
			kind = "synthetic"
		}
		// The budget is for an input of the class size; scale the measured time to it.
		scaled := time.Duration(float64(in.dur) * float64(target) / float64(max(in.size, 1)))
		ok := scaled <= limit
		pass = pass && ok
		fmt.Fprintf(w, "cold %s: %s input %.2f MB in %.2fs %s (wall %.2fs, %.2f MB/s); scaled to %.1f MB: %.2fs, budget %.0fs: %s\n",
			name, kind, float64(in.size)/1e6, in.dur.Seconds(), clock, in.wall.Seconds(), mbps(in.size, in.dur), float64(target)/1e6, scaled.Seconds(), limit.Seconds(), passFail(ok))
	}
	report("8.5 MB class", small, gateSmallLimit, gateSmallBytes)
	report("50 MB class", large, gateLargeLimit, gateLargeBytes)
	// The largest and the slowest inputs: kind, size, time, rate and dominant rule only.
	row := func(in *dryInput) {
		kind := in.ref.Role
		if in.synth != "" {
			kind = "synthetic"
		}
		top := "-"
		if in.top != "" {
			top = fmt.Sprintf("%s %.0f%%", in.top, 100*in.share)
		}
		fmt.Fprintf(w, "  %-18s %9.2f MB %8.2fs %9.2f MB/s  %s\n", kind, float64(in.size)/1e6, in.dur.Seconds(), mbps(in.size, in.dur), top)
	}
	bySize := append([]*dryInput(nil), inputs...)
	sort.Slice(bySize, func(i, j int) bool { return bySize[i].size > bySize[j].size })
	fmt.Fprintln(w, "largest inputs (kind, size, time, rate, dominant rule):")
	for _, in := range bySize[:min(10, len(bySize))] {
		row(in)
	}
	var big []*dryInput
	for _, in := range inputs {
		if in.size >= 1<<20 && in.synth == "" && !in.err {
			big = append(big, in)
		}
	}
	sort.Slice(big, func(i, j int) bool { return mbps(big[i].size, big[i].dur) < mbps(big[j].size, big[j].dur) })
	fmt.Fprintf(w, "slowest real inputs of at least 1 MiB (%d such inputs):\n", len(big))
	for _, in := range big[:min(10, len(big))] {
		row(in)
	}
	worstOK := true
	if len(big) > 0 {
		rate := mbps(big[0].size, big[0].dur)
		at := func(n int64) time.Duration {
			return time.Duration(float64(n) / 1e6 / math.Max(rate, 1e-9) * float64(time.Second))
		}
		worstOK = at(gateSmallBytes) <= gateSmallLimit && at(gateLargeBytes) <= gateLargeLimit
		fmt.Fprintf(w, "worst real density (%.2f MB/s) extrapolated: 8.5 MB in %.1fs (budget 3s), 50 MB in %.1fs (budget 20s): %s\n",
			rate, at(gateSmallBytes).Seconds(), at(gateLargeBytes).Seconds(), passFail(worstOK))
	}
	fmt.Fprintf(w, "gate (class inputs): %s; at the worst real density: %s\n", passFail(pass), passFail(worstOK))
	return nil
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "FAIL"
}

func gb(n int64) float64 { return float64(n) / 1e9 }

func mbps(n int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / 1e6 / d.Seconds()
}

// selectRoots narrows roots to a comma-separated list of root names or directories (a directory is
// scanned as the root it equals, else as claude-projects).
func selectRoots(roots map[string]string, sel string) (map[string]string, error) {
	if sel == "" {
		return roots, nil
	}
	out := map[string]string{}
	for _, root := range strings.Split(sel, ",") {
		if dir, ok := roots[root]; ok {
			out[root] = dir
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		found := false
		for name, dir := range roots {
			if filepath.Clean(dir) == abs {
				out[name], found = dir, true
			}
		}
		if found {
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("--root: %w", err)
		}
		out[RootClaudeProjects] = abs
	}
	return out, nil
}

// dryInputRun runs one input through the builders the importer uses.
func dryInputRun(b *builder, in *dryInput) error {
	if in.synth != "" {
		_, err := b.textPiece(in.synth)
		return err
	}
	f := in.ref
	fh, err := os.Open(f.Path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	if f.Whole {
		data, err := io.ReadAll(fh)
		if err != nil {
			return nil
		}
		_, err = b.fileBody(f, data, func(fileRef) map[[32]byte]bool { return nil })
		if errors.Is(err, errSkip) {
			return nil
		}
		return err
	}
	var cf *codexFile
	if f.Role == RoleCodexRollout {
		if cf, err = readCodexHeader(f.Path); err != nil {
			return err
		}
	}
	rd := bufio.NewReaderSize(fh, 1<<20)
	for n := int64(1); ; n++ {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return nil
		}
		line = line[:len(line)-1]
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if cf != nil {
			_, err = b.codexRecord(f, cf, line, n)
		} else {
			_, err = b.claudeRecord(f, line)
		}
		if err != nil && !errors.Is(err, errSkip) {
			return err
		}
	}
}

// profileInput feeds every decoded string leaf and key of an input to the masker's per-rule profiling
// hook.
func profileInput(p *mask.Masker, in *dryInput, acc map[string]time.Duration) int64 {
	var n int64
	visit := func(s string) {
		p.ProfileRules(s, acc)
		n += int64(len(s))
	}
	if in.synth != "" {
		visit(in.synth)
		return n
	}
	forEachString(in.ref, visit)
	return n
}

// forEachString calls fn with every string value and key of a JSONL file's lines or a JSON file, or
// with a text file's whole content.
func forEachString(f fileRef, fn func(string)) {
	fh, err := os.Open(f.Path)
	if err != nil {
		return
	}
	defer fh.Close()
	if f.Whole && f.Role != FileWorkflowRun && f.Role != FileAgentMeta {
		if data, err := io.ReadAll(fh); err == nil {
			fn(string(data))
		}
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			fn(x)
		case map[string]any:
			for k, e := range x {
				fn(k)
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	rd := bufio.NewReaderSize(fh, 1<<20)
	for {
		ln, err := rd.ReadBytes('\n')
		if len(ln) > 0 {
			if v, derr := decodeAny(ln); derr == nil {
				walk(v)
			}
		}
		if err != nil {
			return
		}
	}
}

// gateInputs picks the inputs whose sizes are nearest the two gate classes: the 8.5 MB class among
// inputs of 4-17 MB and the 50 MB class among inputs of 25-100 MB. A real input always wins over the
// synthetic one, which stands in only when the sample has no real input of the class.
func gateInputs(inputs []*dryInput) (small, large *dryInput) {
	near := func(lo, hi, target int64) *dryInput {
		var best *dryInput
		for _, in := range inputs {
			if in.err || in.size < lo || in.size > hi {
				continue
			}
			realBeatsSynth := best != nil && best.synth != "" && in.synth == ""
			if best == nil || realBeatsSynth || (in.synth != "") == (best.synth != "") && abs64(in.size-target) < abs64(best.size-target) {
				best = in
			}
		}
		return best
	}
	return near(4_000_000, 17_000_000, gateSmallBytes), near(25_000_000, 100_000_000, gateLargeBytes)
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// stratifiedSample picks at most budget bytes of files across strata of (role, month of mtime): the
// largest file of each role first, the files nearest the two gate sizes, then random files per stratum
// in proportion to the stratum's bytes (seeded, so a rerun picks the same sample).
func stratifiedSample(files []fileRef, budget int64, seed uint64) []fileRef {
	r := rand.New(rand.NewPCG(seed, 0x6761))
	picked := map[string]bool{}
	var out []fileRef
	var used int64
	take := func(f fileRef) {
		if picked[f.Key] || used+f.Size > budget {
			return
		}
		picked[f.Key] = true
		out = append(out, f)
		used += f.Size
	}
	byRole := map[string][]fileRef{}
	for _, f := range files {
		byRole[f.Role] = append(byRole[f.Role], f)
	}
	roles := make([]string, 0, len(byRole))
	for role := range byRole {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		var big fileRef
		for _, f := range byRole[role] {
			if f.Size > big.Size {
				big = f
			}
		}
		if big.Key != "" {
			take(big)
		}
	}
	for _, target := range []int64{gateSmallBytes, gateLargeBytes} {
		var best fileRef
		for _, f := range files {
			if f.Whole && f.Role != FileToolResult {
				continue
			}
			if best.Key == "" || abs64(f.Size-target) < abs64(best.Size-target) {
				best = f
			}
		}
		if best.Key != "" {
			take(best)
		}
	}
	strata := map[string][]fileRef{}
	var totalBytes int64
	for _, f := range files {
		if picked[f.Key] {
			continue
		}
		k := f.Role + "/" + time.Unix(0, f.MtimeNS).UTC().Format("2006-01")
		strata[k] = append(strata[k], f)
		totalBytes += f.Size
	}
	keys := make([]string, 0, len(strata))
	for k := range strata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	remaining := budget - used
	for _, k := range keys {
		fs := strata[k]
		var sb int64
		for _, f := range fs {
			sb += f.Size
		}
		share := int64(float64(remaining) * float64(sb) / math.Max(float64(totalBytes), 1))
		r.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] })
		var got int64
		for _, f := range fs {
			if got >= share {
				break
			}
			if got+f.Size > share && got > 0 {
				continue
			}
			before := used
			take(f)
			got += used - before
		}
	}
	return out
}

// placeholderRe matches a mask/v1 placeholder.
var placeholderRe = regexp.MustCompile(`\[masked:([A-Za-z0-9_.-]+)(?::[0-9a-f]{12})?\]`)

// maskReview prints masked spans of one rule with 40 characters of context for the operator's review of
// generic-api-key before the first upload. It shows unmasked text, so it refuses unless stdout is a
// terminal and CHAT_ARCHIVE_MASK_REVIEW_OPERATOR=1 is set. Agents must never run it.
func (e *Env) maskReview(rule string, n int, root string) error {
	if !isTTY(e.Stdout) || e.getenv("CHAT_ARCHIVE_MASK_REVIEW_OPERATOR") != "1" {
		return errors.New("mask-review prints unmasked text and runs only in the operator's own terminal: stdout must be a terminal and CHAT_ARCHIVE_MASK_REVIEW_OPERATOR=1 must be set")
	}
	p := pathsFor(e.Home)
	cfg, err := loadConfig(p)
	if err != nil && !errors.Is(err, errNotEnrolled) {
		return err
	}
	roots := cfg.Roots
	if len(roots) == 0 {
		roots = DefaultRoots(e.Home)
	}
	if roots, err = selectRoots(roots, root); err != nil {
		return err
	}
	files, err := discover(roots)
	if err != nil {
		return err
	}
	m, err := mask.New(mask.Config{})
	if err != nil {
		return err
	}
	rnd := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	var sample []string
	seen := 0
	for _, f := range files {
		forEachString(f, func(s string) {
			out, hits := m.String(s)
			if hits[rule] == 0 {
				return
			}
			for _, sp := range reviewSpans(s, out, rule) {
				seen++
				if len(sample) < n {
					sample = append(sample, sp)
				} else if j := rnd.IntN(seen); j < n {
					sample[j] = sp
				}
			}
		})
	}
	for i, sp := range sample {
		fmt.Fprintf(e.Stdout, "%3d %s\n", i+1, sp)
	}
	fmt.Fprintf(e.Stdout, "%d spans of %s seen, %d shown\n", seen, rule, len(sample))
	return nil
}

// reviewSpans aligns a masked string with its original and returns, for each placeholder of rule, the
// original span with 40 characters of context on each side.
func reviewSpans(orig, out, rule string) []string {
	var res []string
	locs := placeholderRe.FindAllStringSubmatchIndex(out, -1)
	i, j := 0, 0
	for k, loc := range locs {
		lit := out[j:loc[0]]
		if !strings.HasPrefix(orig[i:], lit) {
			return res
		}
		i += len(lit)
		next := len(out)
		if k+1 < len(locs) {
			next = locs[k+1][0]
		}
		after := out[loc[1]:next]
		end := len(orig)
		if after != "" || k+1 < len(locs) {
			idx := strings.Index(orig[i:], after)
			if idx < 0 || after == "" {
				return res
			}
			end = i + idx
		}
		if out[loc[2]:loc[3]] == rule {
			lo, hi := max(0, i-40), min(len(orig), end+40)
			res = append(res, fmt.Sprintf("%q [[%q]] %q", orig[lo:i], orig[i:end], orig[end:hi]))
		}
		i, j = end, loc[1]
	}
	return res
}
