import Foundation

/// When the app tries again after the dashboard page or a readout poll fails.
///
/// One schedule for both, so a page retry and a poll retry never race. A
/// failure with no answer at all retries within seconds, because it usually
/// clears as soon as the network does. Anything else waits minutes: an HTTP
/// error will not change in seconds, and each try is another request. The
/// network coming back, a wake or anything the user does starts over.
///
/// Pure state. The app owns the timer, the path monitor and the wake observer.
public struct RecoverySchedule: Equatable {
    public enum Failure: Equatable {
        /// No answer: offline, no route or name yet, a dropped connection.
        case network
        /// An answer a quick retry will not change: an HTTP error, a redirect
        /// away from the dashboard, or a page process that stopped.
        case server
    }

    /// What a due retry runs again.
    public enum Retry: Equatable {
        case page, poll
    }

    /// Quick steps after no answer. A page keeps trying on the slow steps after them.
    public static let networkDelays: [TimeInterval] = [2, 5, 10, 20, 40]
    /// Slow steps, up to 15 minutes, for everything else.
    public static let slowDelays: [TimeInterval] = [60, 120, 300, 600, 900]
    /// Readout ticks a loaded page may go without its first reading before it
    /// is loaded again.
    public static let stalledTicks = 3
    /// Times a page that never reads anything is loaded again before the app
    /// stops trying until the steps start over. Three fresh documents, on the
    /// 1, 2 and 5-minute steps, get past a page that failed to start once (a
    /// deployment mid-load, say). One that still reads nothing is waiting for
    /// someone, usually signed out, and each further load only downloads the
    /// page again and discards anything typed into it.
    public static let stalledReloads = 3

    /// The retry waiting for its delay, if any.
    public private(set) var pending: Retry?
    private var pageFailures = 0
    private var pollFailures = 0
    private var stalls = 0
    /// Ticks since the page loaded without a reading. Nil once it has one, or
    /// while no page is loaded.
    private var unreadTicks: Int?
    private var held = false

    public init() {}

    /// A page load failed. Returns the delay before loading it again.
    public mutating func pageFailed(_ failure: Failure) -> TimeInterval {
        defer { pageFailures += 1 }
        pending = .page
        unreadTicks = nil
        return Self.step(failure == .network ? Self.networkDelays + Self.slowDelays : Self.slowDelays, pageFailures)
    }

    /// A page load began, by the user or by a due retry.
    public mutating func pageStarted() {
        if pending == .page { pending = nil }
        unreadTicks = nil
    }

    /// A page finished loading. Its failures are forgotten but its stalls are
    /// not, so a page loaded again for never reading anything never reloads
    /// faster, and is loaded again at most `stalledReloads` times.
    public mutating func pageLoaded() {
        pageFailures = 0
        held = false
        unreadTicks = 0
        if pending == .page { pending = nil }
    }

    /// A poll got no answer. Returns the delay before polling again, or nil
    /// once the quick steps are spent (the regular clock is soon enough) or
    /// while a page retry is due anyway.
    public mutating func pollFailed() -> TimeInterval? {
        guard pending != .page, pollFailures < Self.networkDelays.count else { return nil }
        defer { pollFailures += 1 }
        pending = .poll
        return Self.networkDelays[pollFailures]
    }

    /// A poll began.
    public mutating func pollStarted() {
        if pending == .poll { pending = nil }
    }

    /// A reading arrived: the page and its polls work.
    public mutating func received() {
        pollFailures = 0
        stalls = 0
        held = false
        unreadTicks = nil
        if pending == .poll { pending = nil }
    }

    /// A console request in this page got no answer. CPA may have counted it,
    /// and a reload would present the key again, so the page is never loaded
    /// again for lack of a reading until it loads for another reason. A page
    /// that fails (a crash, say) is still retried on its steps, and the fresh
    /// page may present the key once more each time.
    public mutating func consoleUnanswered() {
        held = true
    }

    /// A readout tick polled the loaded page. Returns the delay before loading
    /// it again once it has gone `stalledTicks` ticks without a reading, and
    /// nil after `stalledReloads` such loads until a reading arrives or the
    /// steps start over.
    public mutating func ticked() -> TimeInterval? {
        guard let ticks = unreadTicks.map({ $0 + 1 }), !held, stalls < Self.stalledReloads,
              pending != .page else { return nil }
        guard ticks >= Self.stalledTicks else {
            unreadTicks = ticks
            return nil
        }
        unreadTicks = 0
        pending = .page
        defer { stalls += 1 }
        return Self.step(Self.slowDelays, stalls)
    }

    /// The due retry, which is no longer pending.
    public mutating func take() -> Retry? {
        defer { pending = nil }
        return pending
    }

    /// The network came back, the Mac woke, or the user acted: start the
    /// steps over, which also lets a page that never read anything be loaded
    /// again. Returns a short delay for a retry that was waiting.
    public mutating func restart() -> TimeInterval? {
        pageFailures = 0
        pollFailures = 0
        stalls = 0
        return pending == nil ? nil : Self.networkDelays[0]
    }

    private static func step(_ delays: [TimeInterval], _ index: Int) -> TimeInterval {
        delays[min(index, delays.count - 1)]
    }

    /// Tells the network coming back from the path monitor's other updates.
    ///
    /// Only a usable path after one that was not is the network coming back.
    /// The monitor's first update reports the path as it already was, and it
    /// reports a usable path again whenever its interfaces change (a VPN or
    /// Tailscale connecting alongside Wi-Fi, say). Starting the steps over on
    /// those kept a failing page or poll near the 2-second step.
    public struct NetworkPath: Equatable {
        private var usable: Bool?

        public init() {}

        /// Records an update. True when the network came back.
        public mutating func update(usable: Bool) -> Bool {
            defer { self.usable = usable }
            return usable && self.usable == false
        }
    }
}

extension RecoverySchedule.Failure {
    /// How a failed page load is retried, from its error. Nil for a load the
    /// app ended itself: a newer navigation replaced it, or a policy decision
    /// cancelled it (WebKit's "frame load interrupted") after reporting why.
    public init?(loadErrorDomain domain: String, code: Int) {
        switch (domain, code) {
        case (NSURLErrorDomain, NSURLErrorCancelled), ("WebKitErrorDomain", 102):
            return nil
        case (NSURLErrorDomain, _) where Self.networkCodes.contains(code):
            self = .network
        default:
            self = .server
        }
    }

    private static let networkCodes: Set<Int> = [
        NSURLErrorTimedOut, NSURLErrorCannotFindHost, NSURLErrorCannotConnectToHost,
        NSURLErrorNetworkConnectionLost, NSURLErrorDNSLookupFailed, NSURLErrorNotConnectedToInternet,
    ]
}
