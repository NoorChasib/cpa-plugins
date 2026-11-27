package plugin_test

// K22 (plugin 0.1.6): R1 with production-shaped request bodies. The content pool binds during a
// stall, so this load is shaped by raw body bytes, not envelope counts: request bodies drawn from a
// lognormal with production's 7-day shape (median 390 KiB, p99 2.2 MiB) capped at its largest
// (5.3 MiB), offered at a stated rate of body bytes per minute (production: 32.8 MiB in the p99
// active minute, 90.5 MiB in the peak minute). TestStallR1's production_bodies scenario offers 2 x
// the peak minute through its stall, and fails when this driver is missing or the body bytes or
// steady envelopes offered while the writer was held fall below the scenario's fixed stated rates; the opt-in TestStallR1PoolSustain
// (CA_STALL_POOL_SWEEP=1) stalls at each rate until the first shed and records how long the pool
// sustained it. Every value is synthetic: the bodies are random letters, and only their sizes
// follow production's shape.

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Production's raw body shape over 7 days (council round 12, K22): request bodies, and body bytes
// per active minute (request bodies plus non-stream response bodies).
const (
	prodBodyMedian  = 390 << 10
	prodBodyP99     = 2253 << 10 // 2.2 MiB
	prodBodyMax     = 5557452    // 5.3 MiB
	prodP99MinMiB   = 32.8       // body MiB in the p99 active minute
	prodPeakMinMiB  = 90.5       // body MiB in the peak minute
	prodBodyMinSize = 1 << 10
)

// prodSigma is the lognormal's sigma for prodBodyMedian and prodBodyP99 (z of p99 = 2.3263).
var prodSigma = math.Log(float64(prodBodyP99)/prodBodyMedian) / 2.3263

// prodText is a shared buffer of random letters that bodies are cut from (at varying offsets, each
// prefixed with its request id, so every piece is distinct).
var prodText = sync.OnceValue(func() string {
	rng := rand.New(rand.NewSource(22))
	const alphabet = "abcdefghijklmnopqrstuvwxyz      "
	b := make([]byte, 2*prodBodyMax)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
})

// prodLoad records what a production-shaped driver offered. bytes is the running total of sizes, so
// a reader can take it without the lock (at the writer's release, for example).
type prodLoad struct {
	mu    sync.Mutex
	sizes []int
	start time.Time
	end   time.Time
	bytes atomic.Int64
}

func (p *prodLoad) add(n int) {
	p.mu.Lock()
	p.sizes = append(p.sizes, n)
	p.mu.Unlock()
	p.bytes.Add(int64(n))
}

// summary returns the offered count, total MiB, MiB per minute and the median, p99 and largest body.
func (p *prodLoad) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := append([]int(nil), p.sizes...)
	if len(s) == 0 {
		return "bodies=0"
	}
	sort.Ints(s)
	var total int64
	for _, v := range s {
		total += int64(v)
	}
	mins := p.end.Sub(p.start).Minutes()
	return fmt.Sprintf("bodies=%d body_mib=%.1f mib_per_min=%.1f p50_kib=%d p99_kib=%d max_kib=%d",
		len(s), float64(total)/(1<<20), float64(total)/(1<<20)/max(mins, 1e-9), s[len(s)/2]>>10, s[min(len(s)-1, len(s)*99/100)]>>10, s[len(s)-1]>>10)
}

// bodies offers non-stream exchanges whose request bodies follow production's shape at mibPerMin
// MiB of request body a minute for d (or until stop is closed), in the background. Each exchange
// stores its usual envelopes; the response is small.
func (r *r1Run) bodies(seed int64, mibPerMin float64, d time.Duration, stop <-chan struct{}) *prodLoad {
	rng := rand.New(rand.NewSource(seed))
	text := prodText()
	bps := mibPerMin * (1 << 20) / 60
	p := &prodLoad{start: time.Now()}
	r.loads.Add(1)
	go func() {
		defer r.loads.Done()
		defer func() { p.mu.Lock(); p.end = time.Now(); p.mu.Unlock() }()
		next, end := time.Now(), time.Now().Add(d)
		for time.Now().Before(end) {
			size := int(math.Round(prodBodyMedian * math.Exp(prodSigma*rng.NormFloat64())))
			size = min(max(size, prodBodyMinSize), prodBodyMax)
			off := rng.Intn(len(text) - size)
			p.add(size)
			// The ClaudeBody wrapper adds about 150 bytes to the text.
			r.nonStream(text[off:off+size-200], "synthetic small answer")
			next = next.Add(time.Duration(float64(size) / bps * float64(time.Second)))
			select {
			case <-time.After(time.Until(next)):
			case <-stop:
				return
			case <-r.stop:
				return
			}
		}
	}()
	return p
}

// TestStallR1PoolSustain (CA_STALL_POOL_SWEEP=1) holds another write-connection user's commit for
// as long as the pool takes production-shaped bodies at each rate (CA_STALL_POOL_RATES, MiB a
// minute; default production's p99 minute, its peak minute and 2 x the peak) and records the
// stall's length at the first drop of any kind (stall_sustained), capped at CA_STALL_POOL_MAX
// seconds (default 900). After the release every offered envelope is stored or counted, and a shed
// body's envelope is stored as metadata.
func TestStallR1PoolSustain(t *testing.T) {
	if os.Getenv("CA_STALL_POOL_SWEEP") == "" {
		t.Skip("opt-in: CA_STALL_POOL_SWEEP=1")
	}
	rates := []float64{prodP99MinMiB, prodPeakMinMiB, 2 * prodPeakMinMiB}
	if v := os.Getenv("CA_STALL_POOL_RATES"); v != "" {
		rates = nil
		for _, f := range strings.Split(v, ",") {
			x, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
			if err != nil {
				t.Fatal(err)
			}
			rates = append(rates, x)
		}
	}
	limit := 900 * time.Second
	if v := os.Getenv("CA_STALL_POOL_MAX"); v != "" {
		s, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		limit = time.Duration(s) * time.Second
	}
	for i, rate := range rates {
		t.Run(fmt.Sprintf("%.1f_mib_per_min", rate), func(t *testing.T) {
			r := newR1Run(t)
			e := r.h.Plugin.Engine()
			h, release := r.holdOther()
			stop := make(chan struct{})
			start := time.Now()
			r.inStall.Store(true)
			load := r.bodies(int64(100+i), rate, limit, stop)
			var sustained time.Duration
			var poolAtShed int64
			reason := "limit"
			for tick := time.NewTicker(20 * time.Millisecond); ; <-tick.C {
				if d := nonZeroDrops(e.Stats.Dropped()); len(d) != 0 {
					sustained, poolAtShed = time.Since(start), e.Pool.Used()
					reason = fmt.Sprint(d)
					tick.Stop()
					break
				}
				if time.Since(start) >= limit {
					sustained, poolAtShed = time.Since(start), e.Pool.Used()
					tick.Stop()
					break
				}
			}
			close(stop)
			r.loads.Wait()
			r.inStall.Store(false)
			release()
			stored, offered := r.settleR1()
			_, _, rt := r.h.Plugin.Runtime()
			holdPeak, queuePeak := r1Gauges(rt)
			res := r.result(h, 0)
			t.Logf("rate_mib_per_min=%.1f stall_sustained=%v ended_by=%s pool_at_end_mib=%.1f pool_peak_mib=%.1f hold_peak_kib=%d queue_peak=%d offered=%d stored=%d dropped=%v %s",
				rate, sustained.Round(100*time.Millisecond), reason, float64(poolAtShed)/(1<<20), float64(res.PeakPool)/(1<<20), holdPeak/1024, queuePeak,
				offered, stored, res.Dropped, load.summary())
			if stored != offered {
				t.Fatalf("lost %d of %d envelopes (whole envelopes, not shed bodies), dropped %v", offered-stored, offered, res.Dropped)
			}
		})
	}
}

func nonZeroDrops(d map[string]uint64) map[string]uint64 {
	out := map[string]uint64{}
	for k, v := range d {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}
