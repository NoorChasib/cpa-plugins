import AppKit
import GlanceCore
import WebKit

/// Owns the bridge and its native clock, independently of popover visibility.
/// Credentials are reused inside the web page and never cross this boundary.
@MainActor
final class QuotaReadoutController: NSObject {
    var onChange: (() -> Void)?
    var onRecover: (() -> Void)?
    private(set) var state = QuotaReadoutState()
    private weak var webView: WKWebView?
    private var location: DashboardLocation?
    private var session = UUID().uuidString
    private var timer: Timer?
    private var activity: NSObjectProtocol?
    private var wakeObserver: NSObjectProtocol?
    private var poll = 0
    private var polling = false

    init(webView: WKWebView) {
        self.webView = webView
        super.init()
        webView.configuration.userContentController.add(WeakReadoutHandler(self), name: "quotaGlanceReadout")
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor in
                guard let self, self.timer != nil else { return }
                self.poll += 1
                self.polling = false
                self.onChange?()
                self.refresh()
            }
        }
    }

    func configure(_ location: DashboardLocation) {
        self.location = location
        session = UUID().uuidString
        poll += 1
        polling = false
        state = QuotaReadoutState()
        guard let webView,
              let file = Bundle.main.url(forResource: "QuotaReadout", withExtension: "js"),
              let source = try? String(contentsOf: file, encoding: .utf8),
              let script = Self.script(source, for: location, session: session) else {
            onChange?()
            return
        }
        let content = webView.configuration.userContentController
        content.removeAllUserScripts()
        content.addUserScript(WKUserScript(source: script, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        onChange?()
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
            Task { @MainActor in self?.refresh() }
        }
        clock.tolerance = 5
        RunLoop.main.add(clock, forMode: .common)
        timer = clock
        refresh()
    }

    func stop() {
        setEnabled(false)
        if let wakeObserver { NSWorkspace.shared.notificationCenter.removeObserver(wakeObserver) }
        wakeObserver = nil
    }

    func refreshIfEnabled() {
        if timer != nil { refresh() }
    }

    func markUnavailable() {
        state.markUnavailable()
        onChange?()
    }

    func refresh() {
        // Re-evaluate expiry even if WebKit cannot finish an earlier request.
        onChange?()
        onRecover?()
        guard !polling, let webView, !webView.isLoading,
              let url = webView.url, location?.isDashboard(url) == true else { return }
        polling = true
        poll += 1
        let currentPoll = poll
        webView.callAsyncJavaScript("await window.__quotaGlanceReadout?.refresh()", arguments: [:], in: nil, in: .page) { [weak self] result in
            guard let self, currentPoll == self.poll else { return }
            self.polling = false
            if case .failure = result { self.markUnavailable() }
        }
        // A native deadline remains effective even when WebKit's JS timers or
        // process have stalled. Future native ticks may retry without reloading.
        DispatchQueue.main.asyncAfter(deadline: .now() + 25) { [weak self] in
            guard let self, self.polling, self.poll == currentPoll else { return }
            self.poll += 1
            self.polling = false
            self.markUnavailable()
        }
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
        let expectedPort = location.url.port ?? (location.url.scheme == "https" ? 443 : 80)
        let originPort = message.frameInfo.securityOrigin.port
        guard (originPort == 0 ? (location.url.scheme == "https" ? 443 : 80) : originPort) == expectedPort else { return }
        if envelope.kind == "snapshot", let snapshot = envelope.snapshot {
            state.receive(snapshot)
            onChange?()
        } else if envelope.kind == "unavailable" {
            markUnavailable()
        }
    }

    private struct Envelope: Decodable {
        let session: String
        let kind: String
        let snapshot: QuotaReadoutSnapshot?
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
