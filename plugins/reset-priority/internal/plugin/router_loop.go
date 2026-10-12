package plugin

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	qc "github.com/NoorChasib/cpa-plugins/plugins/quota-cache/client"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/clock"
	"github.com/NoorChasib/cpa-plugins/plugins/reset-priority/internal/route"
)

// routerEvery is the router loop's rebuild interval. route.MaxTableAge is three
// of these, so a table that missed two rebuilds stops routing.
const routerEvery = time.Minute

// Log lines. The router's reason goes in the message, because host.log takes
// no fields here. None ever carries an ID, a header or a panic value.
const (
	logRoutingOn       = "reset-priority API credit routing on"
	logRoutingOff      = "reset-priority API credit routing off"
	logRouterActive    = "reset-priority API credit router active"
	logRouterInactive  = "reset-priority API credit router inactive (%s)"
	logRouterNotBuilt  = "reset-priority API credit router could not be built"
	logRoutingCannotGo = "reset-priority API credit routing is on but cannot route (%s); CPA still sends every request through its slower selection path"
)

// lasting are the table reasons that need the operator, so the loop warns
// once they have held for route.InactiveWarnAfter while the scheduler is
// declared. The others clear on their own: a meter that is unreadable for a
// moment, or one that is saved late.
var lasting = map[route.Reason]bool{
	route.ReasonCreditsMissing:      true, // Quota Cache older than 0.2.0, or it never published
	route.ReasonCreditsUnreadable:   true,
	route.ReasonCreditsStale:        true, // Quota Cache disabled or stopped: this takes over from meter_stopped
	route.ReasonSettingsUnavailable: true,
	route.ReasonMeterMissing:        true,
	route.ReasonMeterStopped:        true,
	route.ReasonMeterTooOld:         true,
	route.ReasonNoAPICredits:        true,
	route.ReasonNoLinkedOrgs:        true,
}

// routerLoop rebuilds the API credit router's table once a minute from Quota
// Cache's published credit state and meter. It runs on the clock's timers,
// never on an engine goroutine, and takes no lock but its own: never the
// engine's, the Runtime's mu, dispatchMu or dispatchWG. Its mu is a leaf,
// never held across a file read, a build or a log line.
type routerLoop struct {
	clk    clock.Clock
	router *route.Router
	log    func(level, message string)
	// beforeBuild is a test-only hook, never set in production. It runs
	// inside the build's recover.
	beforeBuild atomic.Pointer[func()]

	mu sync.Mutex
	// gen identifies the current loop: start and stop bump it, so a timer or
	// a run left from an earlier configure does nothing.
	gen      uint64
	running  bool
	timer    clock.Timer
	path     string // the Quota Cache snapshot path
	declared bool   // the scheduler is declared, so the lasting warning applies
	// Per configure: start resets them.
	builtWarned, lastingWarned bool
	inactive                   route.Reason // the table reason being timed, or ReasonNone
	inactiveSince              time.Time

	// inFlight counts runs that have started and not finished, and idle is
	// signalled when it drops to 0, so stop can wait for them.
	inFlight int
	idle     *sync.Cond
}

func newRouterLoop(clk clock.Clock, router *route.Router, log func(level, message string)) *routerLoop {
	l := &routerLoop{clk: clk, router: router, log: log}
	l.idle = sync.NewCond(&l.mu)
	return l
}

// start (re)starts the loop on the snapshot path with an immediate build, and
// arms the warnings again. A run of the previous configure finishes first.
func (l *routerLoop) start(path string, declared bool) {
	l.stop()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	l.running = true
	l.path, l.declared = path, declared
	l.builtWarned, l.lastingWarned = false, false
	l.inactive, l.inactiveSince = route.ReasonNone, time.Time{}
	l.armLocked(0)
}

// stop ends the loop and waits for a run in flight, so nothing publishes
// after it returns. It is idempotent, and safe before the first start.
func (l *routerLoop) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	l.running = false
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	for l.inFlight > 0 {
		l.idle.Wait()
	}
}

// setDeclared records whether the scheduler is declared, which the lasting
// warning needs. A configure clears it on entry, before it can fail.
func (l *routerLoop) setDeclared(on bool) {
	l.mu.Lock()
	l.declared = on
	l.mu.Unlock()
}

// armLocked schedules the next run of the current generation. Caller holds mu.
func (l *routerLoop) armLocked(after time.Duration) {
	gen := l.gen
	l.timer = l.clk.AfterFunc(after, func() { l.run(gen) })
}

// run is one rebuild. It does nothing when its generation has ended, and arms
// the next one when it has not, whatever the build did.
func (l *routerLoop) run(gen uint64) {
	l.mu.Lock()
	if !l.running || l.gen != gen {
		l.mu.Unlock()
		return
	}
	path := l.path
	l.inFlight++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.running && l.gen == gen {
			l.armLocked(routerEvery)
		}
		if l.inFlight--; l.inFlight == 0 {
			l.idle.Broadcast()
		}
	}()

	l.build(path, l.clk.Now().UTC())
}

// build reads Quota Cache's two files, builds the table and publishes it. A
// panic here would abort CPA, because no goroutine above it recovers, so it
// steps the router aside with build_failed instead and warns once per
// configure without the panic value, which can carry meter data.
func (l *routerLoop) build(path string, now time.Time) {
	defer func() {
		if recover() != nil {
			l.router.BuildFailed(now)
			if l.once(&l.builtWarned) {
				l.log("warn", logRouterNotBuilt)
			}
		}
	}()
	in := route.Input{Now: now}
	state, err := qc.LoadCredits(qc.CreditsPath(path))
	in.CreditsError = loadError(err, qc.ErrCreditsMissing)
	if in.CreditsError == "" {
		in.Credits = qc.CreditsAt(state, now, route.MaxMeterAge)
	}
	meter, err := qc.LoadMeter(qc.MeterPath(path))
	in.MeterError = loadError(err, qc.ErrMeterMissing)
	if in.MeterError == "" {
		in.Meter = meter
	}
	if hook := l.beforeBuild.Load(); hook != nil {
		(*hook)()
	}
	table := route.Build(in)
	wasActive, isActive := l.router.Publish(table, now)
	switch {
	case isActive && !wasActive:
		l.log("info", logRouterActive)
	case wasActive && !isActive:
		reason := table.Reason
		if reason == route.ReasonNone {
			reason = route.ReasonDisabled
		}
		l.log("info", fmt.Sprintf(logRouterInactive, reason.String()))
	}
	l.warnIfLasting(table.Reason, now)
}

// warnIfLasting times the table's reason and warns once per configure when a
// lasting one has held for route.InactiveWarnAfter while the scheduler is
// declared: CPA then sends every request through its legacy selection path
// for nothing.
func (l *routerLoop) warnIfLasting(reason route.Reason, now time.Time) {
	l.mu.Lock()
	if reason != l.inactive {
		l.inactive, l.inactiveSince = reason, now
	}
	warn := l.declared && !l.lastingWarned && lasting[reason] && now.Sub(l.inactiveSince) >= route.InactiveWarnAfter
	if warn {
		l.lastingWarned = true
	}
	l.mu.Unlock()
	if warn {
		l.log("warn", fmt.Sprintf(logRoutingCannotGo, reason.String()))
	}
}

// once reports whether flag was false, setting it.
func (l *routerLoop) once(flag *bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := !*flag
	*flag = true
	return first
}

// loadError is the router's name for a Quota Cache load error: "" when it
// loaded, "missing" when there is no file, "unreadable" otherwise.
func loadError(err, missing error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, missing):
		return "missing"
	default:
		return "unreadable"
	}
}
