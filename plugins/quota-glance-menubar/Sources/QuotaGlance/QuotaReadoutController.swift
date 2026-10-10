import AppKit
import GlanceCore
import Network
import WebKit

/// Owns the bridge and its native clock, independently of popover visibility.
/// Credentials are reused inside the web page and never cross this boundary.
@MainActor
final class QuotaReadoutController: NSObject {
    var onChange: (() -> Void)?
    /// The recovery schedule wants the dashboard loaded again: its load
    /// failed, or it loaded and never produced a reading.
    var onRecover: (() -> Void)?
    private(set) var state = QuotaReadoutState()
    private(set) var recovery = RecoverySchedule()
    private weak var webView: WKWebView?
    private var location: DashboardLocation?
    private var session = UUID().uuidString
    private var timer: Timer?
    private var deadline: Timer?
    private var retry: Timer?
    private let pathMonitor = NWPathMonitor()
    /// Outlives `recovery`, which a new dashboard replaces: the network's
    /// state does not change with the address.
    private var networkPath = RecoverySchedule.NetworkPath()
    private var activity: NSObjectProtocol?
    private var wakeObserver: NSObjectProtocol?
    private var poll = 0
    private var polling = false
    /// Settings asked for the window list while the clock is off (Icon only):
    /// one refresh, now or when the page next finishes loading.
    private var windowsRequested = false

    init(webView: WKWebView) {
        self.webView = webView
        super.init()
        webView.configuration.userContentController.add(WeakReadoutHandler(self), name: "quotaGlanceReadout")
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.restartRecovery()
                guard self.timer != nil else { return }
                self.poll += 1
                self.polling = false
                self.onChange?()
                self.refresh()
            }
        }
        // The network coming back is only a reason to retry sooner; a VPN
        // joining a working connection is not that (networkPathChanged). Polls
        // never wait for a usable path: a dashboard on this Mac needs none.
        pathMonitor.pathUpdateHandler = { [weak self] path in
            let usable = path.status == .satisfied
            Task { @MainActor [weak self] in self?.networkPathChanged(usable: usable) }
        }
        pathMonitor.start(queue: .main)
    }

    /// Starts the steps over only when the network comes back, not on every
    /// report of a usable path (RecoverySchedule.NetworkPath).
    func networkPathChanged(usable: Bool) {
        guard networkPath.update(usable: usable) else { return }
        restartRecovery()
    }

    func configure(_ location: DashboardLocation, scriptSource: String? = nil) {
        self.location = location
        session = UUID().uuidString
        poll += 1
        polling = false
        state = QuotaReadoutState()
        recovery = RecoverySchedule()
        settleRetry()
        guard let webView,
              let source = scriptSource ?? Self.bundledScript(),
              let script = Self.script(source, for: location, session: session) else {
            onChange?()
            return
        }
        let content = webView.configuration.userContentController
        content.removeAllUserScripts()
        content.addUserScript(WKUserScript(source: script, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        onChange?()
    }

    private static func bundledScript() -> String? {
        guard let file = Bundle.main.url(forResource: "QuotaReadout", withExtension: "js") else { return nil }
        return try? String(contentsOf: file, encoding: .utf8)
    }

    static func script(_ source: String, for location: DashboardLocation, session: String) -> String? {
        let url = location.url
        var components = URLComponents(url: url, resolvingAgainstBaseURL: false)!
        components.scheme = components.scheme?.lowercased()
        components.host = components.host?.lowercased()
        if components.port == (components.scheme == "https" ? 443 : 80) { components.port = nil }
        components.path = ""
        components.query = nil
        components.fragment = nil
        let origin = components.url!.absoluteString.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let path = URLComponents(url: url, resolvingAgainstBaseURL: false)!.percentEncodedPath
        let dashboardPath = path.isEmpty ? "/" : path
        let marker = path.range(of: "/v0/")
        let prefix = marker.map { String(path[..<$0.lowerBound]) } ?? ""
        let paths = ["management", "resource"].map { "\(prefix)/v0/\($0)/plugins/quota-glance/summary" }
        let config: [String: Any] = ["origin": origin, "path": dashboardPath, "summaryPaths": paths, "session": session]
        guard let data = try? JSONSerialization.data(withJSONObject: config),
              let json = String(data: data, encoding: .utf8) else { return nil }
        return source.replacingOccurrences(of: "__QUOTA_GLANCE_CONFIG__", with: json)
    }

    func setEnabled(_ enabled: Bool) {
        if !enabled {
            timer?.invalidate()
            timer = nil
            if let activity { ProcessInfo.processInfo.endActivity(activity) }
            activity = nil
            return
        }
        guard timer == nil else { return }
        // The visible, user-selected status readout is background work requested
        // by the user. Permit normal system sleep; no activity for icon-only mode.
        activity = ProcessInfo.processInfo.beginActivity(options: .userInitiatedAllowingIdleSystemSleep,
                                                        reason: "Update the selected menu bar quota")
        let clock = Timer(timeInterval: 60, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in self?.refresh() }
        }
        clock.tolerance = 6
        RunLoop.main.add(clock, forMode: .common)
        timer = clock
        refresh()
    }

    func stop() {
        setEnabled(false)
        if let wakeObserver { NSWorkspace.shared.notificationCenter.removeObserver(wakeObserver) }
        wakeObserver = nil
        pathMonitor.cancel()
        retry?.invalidate()
        retry = nil
        deadline?.invalidate()
        deadline = nil
    }

    func refreshIfEnabled() {
        if timer != nil || windowsRequested { refresh() }
    }

    /// Loads the summary's window list once without starting the clock, so
    /// Settings can offer windows to someone on Icon only.
    func requestWindows() {
        guard !state.hasSummary else { return }
        windowsRequested = true
        refresh()
    }

    func markUnavailable() {
        state.markUnavailable()
        onChange?()
    }

    // The dashboard reports its page loads here, so page and poll retries
    // share one schedule (RecoverySchedule).

    func pageLoadStarted() {
        recovery.pageStarted()
        settleRetry()
    }

    func pageDidLoad() {
        recovery.pageLoaded()
        settleRetry()
        refreshIfEnabled()
    }

    func pageDidFail(_ failure: RecoverySchedule.Failure) {
        arm(recovery.pageFailed(failure))
    }

    /// The network came back, the Mac woke, or the user acted.
    func restartRecovery() {
        arm(recovery.restart())
    }

    /// Starts the timer for the schedule's pending retry, replacing any other.
    private func arm(_ delay: TimeInterval?) {
        guard let delay else { return }
        retry?.invalidate()
        let timer = Timer(timeInterval: delay, repeats: false) { [weak self] _ in
            Task { @MainActor [weak self] in self?.runRetry() }
        }
        timer.tolerance = delay / 10
        RunLoop.main.add(timer, forMode: .common)
        retry = timer
    }

    /// Drops the timer once nothing waits for it, so it costs no wake.
    private func settleRetry() {
        guard recovery.pending == nil else { return }
        retry?.invalidate()
        retry = nil
    }

    private func runRetry() {
        retry = nil
        switch recovery.take() {
        case .page?: onRecover?()
        case .poll?: refresh()
        case nil: break
        }
    }

    func refresh() {
        // Re-evaluate expiry even if WebKit cannot finish an earlier request.
        onChange?()
        guard !polling, let webView, !webView.isLoading,
              let url = webView.url, location?.isDashboard(url) == true else { return }
        polling = true
        poll += 1
        let currentPoll = poll
        recovery.pollStarted()
        settleRetry()
        // A page whose first read failed while hidden, where its own retries
        // pause, would otherwise show nothing until opened.
        arm(recovery.ticked())
        webView.callAsyncJavaScript("""
            if (!window.__quotaGlanceReadout) throw new Error('Readout unavailable');
            await window.__quotaGlanceReadout.refresh();
            return true;
            """, arguments: [:], in: nil, in: .page) { [weak self] result in
            guard let self, currentPoll == self.poll else { return }
            self.polling = false
            self.deadline?.invalidate()
            self.deadline = nil
            if case .failure = result { self.markUnavailable() }
        }
        // A native deadline remains effective even when WebKit's JS timers or
        // process have stalled. Future native ticks may retry without reloading.
        // A timer, unlike a dispatched block, is gone once cancelled: a poll
        // that finishes costs no second wake.
        let limit = Timer(timeInterval: 25, repeats: false) { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self, self.polling, self.poll == currentPoll else { return }
                self.poll += 1
                self.polling = false
                self.deadline = nil
                self.markUnavailable()
            }
        }
        limit.tolerance = 2.5
        RunLoop.main.add(limit, forMode: .common)
        deadline?.invalidate()
        deadline = limit
    }

    fileprivate func receive(_ message: WKScriptMessage) {
        guard message.webView === webView, message.frameInfo.isMainFrame,
              let location, let frameURL = message.frameInfo.request.url,
              location.isDashboard(frameURL),
              let configuredHost = location.url.host?.lowercased(),
              message.frameInfo.securityOrigin.host.lowercased() == configuredHost,
              message.frameInfo.securityOrigin.protocol.lowercased() == location.url.scheme?.lowercased(),
              let data = try? JSONSerialization.data(withJSONObject: message.body),
              let envelope = try? JSONDecoder().decode(Envelope.self, from: data),
              envelope.session == session else { return }
        let expectedPort = location.url.port ?? (location.url.scheme?.lowercased() == "https" ? 443 : 80)
        let originPort = message.frameInfo.securityOrigin.port
        guard (originPort == 0 ? (location.url.scheme?.lowercased() == "https" ? 443 : 80) : originPort) == expectedPort else { return }
        if envelope.kind == "snapshot", let snapshot = envelope.snapshot {
            state.receive(snapshot)
            if state.hasSummary { windowsRequested = false }
            recovery.received()
            settleRetry()
            onChange?()
        } else if envelope.kind == "unavailable" {
            switch envelope.reason {
            // The poll got no answer, or the server did not answer before a
            // console request was sent: try again in seconds, not at the next tick.
            case "network": arm(recovery.pollFailed())
            // A console request got no answer, and CPA may have counted it.
            // Reloading the page would present that key again.
            case "unanswered": recovery.consoleUnanswered()
            default: break
            }
            markUnavailable()
        }
    }

    private struct Envelope: Decodable {
        let session: String
        let kind: String
        let snapshot: QuotaReadoutSnapshot?
        let reason: String?
    }
}

/// WKUserContentController retains its handlers; do not retain the app through it.
@MainActor
private final class WeakReadoutHandler: NSObject, WKScriptMessageHandler {
    weak var owner: QuotaReadoutController?
    init(_ owner: QuotaReadoutController) { self.owner = owner }
    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        owner?.receive(message)
    }
}
