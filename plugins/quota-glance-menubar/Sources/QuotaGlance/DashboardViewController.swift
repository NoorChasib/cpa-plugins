import AppKit
import GlanceCore
import WebKit

@MainActor
final class DashboardViewController: NSViewController, WKNavigationDelegate, WKUIDelegate {
    var onSettings: (() -> Void)?
    private var location: DashboardLocation?
    private var hasDocument = false
    private var failed = false
    private let webView: WKWebView
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
        view = NSView(frame: NSRect(x: 0, y: 0, width: 400, height: 620))
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
        self.location = location
        hasDocument = false
        goToDashboard()
    }

    func goToDashboard() {
        guard let location else { return }
        _ = view
        if failed || !hasDocument { showLoading() }
        failed = false
        webView.load(URLRequest(url: location.url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 30))
    }

    func prepareToShow() {
        // A new document picks up hosted page updates. Do not interrupt an
        // in-progress navigation or a CPA console sign-in on another path.
        guard !webView.isLoading else { return }
        if failed || webView.url == nil {
            goToDashboard()
        } else if let current = webView.url, location?.isDashboard(current) == true {
            reloadPage()
        }
    }

    func reloadPage() {
        if failed || webView.url == nil { goToDashboard() } else { webView.reloadFromOrigin() }
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
    }

    private func showError(_ message: String) {
        failed = true
        webView.isHidden = true
        statusView.isHidden = false
        spinner.stopAnimation(nil)
        spinner.isHidden = true
        statusTitle.stringValue = "Couldn’t open your dashboard"
        statusMessage.stringValue = message
        retryButton.isHidden = false
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        guard !failed else { return }
        hasDocument = true
        statusView.isHidden = true
        webView.isHidden = false
        spinner.stopAnimation(nil)
    }

    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        handleFailure(error)
    }

    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
        handleFailure(error)
    }

    private func handleFailure(_ error: Error) {
        let error = error as NSError
        guard !(error.domain == NSURLErrorDomain && error.code == NSURLErrorCancelled) else { return }
        // Avoid displaying URLs or query parameters in WebKit's raw errors.
        showError("Check your connection and dashboard URL, then try again. If you use a VPN, check that it’s connected.")
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
