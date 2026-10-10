import AppKit
import GlanceCore
import WebKit

@MainActor
final class DashboardViewController: NSViewController, WKNavigationDelegate, WKUIDelegate {
    var onSettings: (() -> Void)?
    /// Escape that nothing in the popover used: the page left it unhandled,
    /// or the page is not showing.
    var onEscape: (() -> Void)?
    private var location: DashboardLocation?
    private var hasDocument = false
    private var failed = false
    private let webView: WKWebView
    private(set) lazy var readout = QuotaReadoutController(webView: webView)
    private let statusView = NSView()
    private let statusTitle = NSTextField(labelWithString: "Loading your dashboard…")
    private let statusMessage = NSTextField(wrappingLabelWithString: "")
    private let spinner = NSProgressIndicator()
    private lazy var retryButton = NSButton(title: "Try Again", target: self, action: #selector(retry))
    private lazy var settingsButton = NSButton(title: "Settings…", target: self, action: #selector(openSettings))

    init(webView suppliedWebView: WKWebView? = nil) {
        let configuration = WKWebViewConfiguration()
        configuration.websiteDataStore = .default()
        webView = suppliedWebView ?? WKWebView(frame: .zero, configuration: configuration)
        super.init(nibName: nil, bundle: nil)
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.underPageBackgroundColor = NSColor(calibratedRed: 16 / 255, green: 17 / 255, blue: 20 / 255, alpha: 1)
    }

    required init?(coder: NSCoder) { fatalError("Use init()") }

    override func loadView() {
        view = FocusableView(frame: NSRect(x: 0, y: 0, width: 400, height: 620))
        view.wantsLayer = true
        view.layer?.backgroundColor = webView.underPageBackgroundColor.cgColor
        for child in [webView, statusView] {
            child.translatesAutoresizingMaskIntoConstraints = false
            view.addSubview(child)
            NSLayoutConstraint.activate([
                child.leadingAnchor.constraint(equalTo: view.leadingAnchor),
                child.trailingAnchor.constraint(equalTo: view.trailingAnchor),
                child.topAnchor.constraint(equalTo: view.topAnchor),
                child.bottomAnchor.constraint(equalTo: view.bottomAnchor),
            ])
        }
        statusTitle.font = .systemFont(ofSize: 17, weight: .semibold)
        statusTitle.alignment = .center
        statusMessage.font = .systemFont(ofSize: 12)
        statusMessage.textColor = .secondaryLabelColor
        statusMessage.alignment = .center
        spinner.style = .spinning
        spinner.controlSize = .small
        let actions = NSStackView(views: [retryButton, settingsButton])
        actions.spacing = 10
        let stack = NSStackView(views: [spinner, statusTitle, statusMessage, actions])
        stack.orientation = .vertical
        stack.alignment = .centerX
        stack.spacing = 14
        stack.translatesAutoresizingMaskIntoConstraints = false
        statusView.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.centerYAnchor.constraint(equalTo: statusView.centerYAnchor),
            stack.leadingAnchor.constraint(equalTo: statusView.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: statusView.trailingAnchor, constant: -24),
            statusMessage.widthAnchor.constraint(equalTo: stack.widthAnchor),
        ])
    }

    func configure(_ location: DashboardLocation) {
        guard self.location != location else { return }
        self.location = location
        readout.configure(location)
        readout.onRecover = { [weak self] in self?.recover() }
        hasDocument = false
        goToDashboard()
    }

    func goToDashboard() {
        readout.restartRecovery()
        load()
    }

    /// Show Dashboard returns to the dashboard but leaves it alone while it is
    /// showing, so the menu item costs no reload. Reload Page reloads.
    func showDashboard() {
        readout.restartRecovery()
        guard let location else { return }
        if !hasDocument || failed || webView.url.map({ !location.isDashboard($0) }) ?? true {
            load()
        }
    }

    func prepareToShow() {
        // Opening is presentation only: keep scroll, forms, and the document.
        // Explicit Reload Page still picks up deployments when requested.
        focusContent()
        readout.restartRecovery()
        guard !webView.isLoading else { return }
        if failed || webView.url == nil {
            load()
        }
    }

    func reloadPage() {
        readout.restartRecovery()
        if failed || webView.url == nil {
            load()
        } else {
            readout.pageLoadStarted()
            webView.reloadFromOrigin()
        }
    }

    private func load() {
        guard let location else { return }
        _ = view
        if failed || !hasDocument { showLoading() }
        failed = false
        readout.pageLoadStarted()
        // The page is served no-cache with an ETag, so WebKit revalidates it:
        // a 304 while it is unchanged, the new page after a deployment.
        webView.load(URLRequest(url: location.url, cachePolicy: .useProtocolCachePolicy, timeoutInterval: 30))
    }

    /// Due on the recovery schedule. A page that failed is loaded again; one
    /// that loaded without a reading only while hidden, since a visible page
    /// retries its own reads and may be in use.
    private func recover() {
        guard let location, !webView.isLoading else { return }
        if failed || webView.url == nil {
            load()
        } else if let url = webView.url, location.isDashboard(url), view.window?.isVisible != true {
            load()
        }
    }

    @objc private func retry() { goToDashboard() }
    @objc private func openSettings() { onSettings?() }

    private func showLoading() {
        webView.isHidden = true
        statusView.isHidden = false
        spinner.isHidden = false
        spinner.startAnimation(nil)
        statusTitle.stringValue = "Loading your dashboard…"
        statusMessage.stringValue = "Connecting to your Quota Glance page."
        retryButton.isHidden = true
        focusContent()
    }

    /// Network failures retry within seconds; the rest on the slow steps, as
    /// an HTTP error page will not change in seconds and each try is a request.
    private func showError(_ message: String, _ failure: RecoverySchedule.Failure = .server) {
        // One failure per attempt, so a load reported twice does not skip a step.
        if !failed { readout.pageDidFail(failure) }
        failed = true
        readout.markUnavailable()
        webView.isHidden = true
        statusView.isHidden = false
        spinner.stopAnimation(nil)
        spinner.isHidden = true
        statusTitle.stringValue = "Couldn’t open your dashboard"
        statusMessage.stringValue = message
        retryButton.isHidden = false
        focusContent()
    }

    /// Keys go to the page while it shows, so its dialogs get Escape first;
    /// otherwise to the root view, which passes them to this controller.
    private func focusContent() {
        guard let window = view.window else { return }
        window.makeFirstResponder(webView.isHidden ? view : webView)
    }

    // WebKit gives the page every key first. An Escape the page left unhandled
    // comes back up the responder chain from the web view as cancelOperation:
    // (WebPageMac.mm runs that command at the keypress); keyDown gets Escape
    // while the loading or error view has focus, or if WebKit re-sends the key
    // (WebViewImpl::doneWithKeyEvent). WebKit's own close of a page dialog
    // leaves the key unhandled, so the bridge (QuotaReadout.js) closes the
    // dialog itself and keeps Escape, and the popover stays open.
    override func keyDown(with event: NSEvent) {
        if event.keyCode == 53, let onEscape {
            onEscape()
        } else {
            super.keyDown(with: event)
        }
    }

    // Escape or Command-period as a command, which is how the page's
    // unhandled Escape arrives.
    override func cancelOperation(_ sender: Any?) {
        onEscape?()
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        guard !failed else { return }
        hasDocument = true
        statusView.isHidden = true
        webView.isHidden = false
        spinner.stopAnimation(nil)
        focusContent()
        readout.pageDidLoad()
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        handleFailure(error)
    }

    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
        handleFailure(error)
    }

    private func handleFailure(_ error: Error) {
        let error = error as NSError
        // Nil when this controller ended the load itself, and has shown why.
        guard let failure = RecoverySchedule.Failure(loadErrorDomain: error.domain, code: error.code) else { return }
        // Avoid displaying URLs or query parameters in WebKit's raw errors.
        showError("Check your connection and dashboard URL, then try again. If you use a VPN, check that it’s connected.", failure)
    }

    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        showError("The page stopped responding. Try again to reopen it.")
    }

    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let location, let url = navigationAction.request.url else {
            decisionHandler(.cancel)
            return
        }
        switch location.navigation(to: url, userActivatedLink: navigationAction.navigationType == .linkActivated) {
        case .embedded:
            if navigationAction.targetFrame == nil {
                decisionHandler(.cancel)
                webView.load(navigationAction.request)
            } else {
                decisionHandler(.allow)
            }
        case .browser:
            decisionHandler(.cancel)
            NSWorkspace.shared.open(url)
        case .blocked:
            decisionHandler(.cancel)
            if navigationAction.targetFrame?.isMainFrame != false {
                showError("This page tried to leave the configured server. If your dashboard has moved, update its URL in Settings.")
            }
        }
    }

    func webView(_ webView: WKWebView, decidePolicyFor navigationResponse: WKNavigationResponse,
                 decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        // Also check the final response URL: redirect handling must not rely
        // solely on whether WebKit emits another navigation-action callback.
        if navigationResponse.isForMainFrame,
           let url = navigationResponse.response.url, location?.contains(url) != true {
            decisionHandler(.cancel)
            showError("The server redirected to a different address. Update the dashboard URL in Settings.")
            return
        }
        if navigationResponse.isForMainFrame,
           let response = navigationResponse.response as? HTTPURLResponse,
           response.statusCode >= 400 {
            decisionHandler(.cancel)
            showError("The server returned HTTP \(response.statusCode). Check the dashboard URL and that Quota Glance is enabled.")
        } else {
            decisionHandler(.allow)
        }
    }

    // HTML <dialog> works directly in WebKit. These cover browser dialogs too,
    // keeping future page changes usable without a second UI implementation.
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        guard let location, let url = navigationAction.request.url else { return nil }
        switch location.navigation(to: url, userActivatedLink: navigationAction.navigationType == .linkActivated) {
        case .embedded: webView.load(navigationAction.request)
        case .browser: NSWorkspace.shared.open(url)
        case .blocked: break
        }
        return nil
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let alert = pageAlert(message, frame: frame)
        alert.addButton(withTitle: "OK")
        alert.runModal()
        completionHandler()
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let alert = pageAlert(message, frame: frame)
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        completionHandler(alert.runModal() == .alertFirstButtonReturn)
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String,
                 defaultText: String?, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (String?) -> Void) {
        let alert = pageAlert(prompt, frame: frame)
        let input = NSTextField(string: defaultText ?? "")
        input.frame = NSRect(x: 0, y: 0, width: 280, height: 24)
        alert.accessoryView = input
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        alert.window.initialFirstResponder = input
        completionHandler(alert.runModal() == .alertFirstButtonReturn ? input.stringValue : nil)
    }

    private func pageAlert(_ message: String, frame: WKFrameInfo) -> NSAlert {
        let alert = NSAlert()
        alert.messageText = frame.request.url?.host ?? "Dashboard"
        alert.informativeText = message
        NSApp.activate(ignoringOtherApps: true)
        return alert
    }
}

/// Takes focus while the status view shows, so Escape still reaches the controller.
private final class FocusableView: NSView {
    override var acceptsFirstResponder: Bool { true }
}
