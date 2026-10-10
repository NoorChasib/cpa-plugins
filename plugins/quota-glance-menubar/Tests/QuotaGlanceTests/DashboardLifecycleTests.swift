import AppKit
import GlanceCore
import WebKit
import XCTest
@testable import QuotaGlance

@MainActor
private final class RecordingWebView: WKWebView {
    var pageURL: URL?
    var stubLoading = false
    var loads = 0
    var reloads = 0
    var lastRequest: URLRequest?
    override var url: URL? { pageURL }
    override var isLoading: Bool { stubLoading }
    override func load(_ request: URLRequest) -> WKNavigation? {
        loads += 1
        lastRequest = request
        return nil
    }
    override func reloadFromOrigin() -> WKNavigation? {
        reloads += 1
        return nil
    }
}

final class DashboardLifecycleTests: XCTestCase {
    @MainActor
    func testReopeningLoadedDashboardNeverNavigatesOrReloads() async throws {
        _ = NSApplication.shared
        let location = try DashboardLocation("https://quota.example.com/v0/resource/plugins/quota-glance/app")
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        controller.configure(location)
        webView.pageURL = location.url
        controller.prepareToShow()
        controller.prepareToShow()
        controller.configure(location) // Saving only the quota must preserve the page too.
        XCTAssertEqual(webView.loads, 1, "Reopening must preserve the existing document and scroll position")
        XCTAssertEqual(webView.reloads, 0, "Opening the menu must not refresh the page")
    }

    @MainActor
    func testExplicitReloadStillReloadsThePage() async throws {
        _ = NSApplication.shared
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        let location = try DashboardLocation("https://quota.example.com/app")
        controller.configure(location)
        webView.pageURL = location.url
        controller.reloadPage()
        XCTAssertEqual(webView.reloads, 1)
    }

    @MainActor
    func testOpeningRetriesFailedLoadWithoutInterruptingActiveNavigation() async throws {
        _ = NSApplication.shared
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        controller.configure(try DashboardLocation("https://quota.example.com/app"))
        webView.stubLoading = true
        controller.prepareToShow()
        XCTAssertEqual(webView.loads, 1)
        webView.stubLoading = false
        controller.webView(webView, didFailProvisionalNavigation: nil, withError: URLError(.notConnectedToInternet))
        controller.prepareToShow()
        XCTAssertEqual(webView.loads, 2)
    }

    @MainActor
    func testDashboardLoadRevalidatesTheCachedPage() async throws {
        _ = NSApplication.shared
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        controller.configure(try DashboardLocation("https://quota.example.com/app"))
        // The page is served no-cache with an ETag: an unchanged page is a 304.
        XCTAssertEqual(webView.lastRequest?.cachePolicy, .useProtocolCachePolicy)
    }

    @MainActor
    func testShowDashboardLeavesTheShowingDashboardAlone() async throws {
        _ = NSApplication.shared
        let location = try DashboardLocation("https://quota.example.com/cpa/v0/resource/plugins/quota-glance/app")
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        controller.configure(location)
        webView.pageURL = location.url
        controller.showDashboard()
        XCTAssertEqual(webView.loads, 2, "Before the first document there is nothing to keep")
        controller.webView(webView, didFinish: nil)
        controller.showDashboard()
        XCTAssertEqual(webView.loads, 2, "Show Dashboard on the dashboard must not reload it")
        XCTAssertEqual(webView.reloads, 0)
        webView.pageURL = URL(string: "https://quota.example.com/cpa/management.html")
        controller.showDashboard()
        XCTAssertEqual(webView.loads, 3, "From another console page it returns to the dashboard")
        webView.pageURL = location.url
        controller.webView(webView, didFinish: nil)
        controller.webView(webView, didFailProvisionalNavigation: nil, withError: URLError(.timedOut))
        controller.showDashboard()
        XCTAssertEqual(webView.loads, 4, "A failed load is tried again")
    }

    @MainActor
    func testFailuresAreScheduledOnceAndItsOwnCancelsNotAtAll() async throws {
        _ = NSApplication.shared
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        controller.configure(try DashboardLocation("https://quota.example.com/app"))
        // WebKit reports cancelling an HTTP error response, already shown and scheduled.
        controller.webView(webView, didFailProvisionalNavigation: nil, withError: NSError(domain: "WebKitErrorDomain", code: 102))
        XCTAssertNil(controller.readout.recovery.pending)
        controller.webViewWebContentProcessDidTerminate(webView)
        XCTAssertEqual(controller.readout.recovery.pending, .page)
        // The same attempt also failing its navigation must not skip a step.
        controller.webView(webView, didFail: nil, withError: NSError(domain: "WKErrorDomain", code: 2))
        var next = controller.readout.recovery
        XCTAssertEqual(next.pageFailed(.server), 120)
    }

    // A VPN or Tailscale changing interfaces reports a usable path again, and
    // each report used to start the steps over, so an HTTP error was retried
    // within seconds instead of minutes.
    @MainActor
    func testOnlyTheNetworkComingBackStartsTheStepsOver() async throws {
        _ = NSApplication.shared
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let readout = QuotaReadoutController(webView: webView)
        defer { readout.stop() }
        readout.configure(try DashboardLocation("https://quota.example.com/app"))
        readout.pageDidFail(.server)
        readout.pageDidFail(.server)
        readout.networkPathChanged(usable: true) // The monitor's first report.
        readout.networkPathChanged(usable: true)
        var next = readout.recovery
        XCTAssertEqual(next.pageFailed(.server), 300, "A usable path reported again is not the network coming back")
        readout.networkPathChanged(usable: false)
        readout.networkPathChanged(usable: true)
        next = readout.recovery
        XCTAssertEqual(next.pageFailed(.server), 60)
        // A new dashboard does not forget that the network was down.
        readout.networkPathChanged(usable: false)
        readout.configure(try DashboardLocation("https://other.example.com/app"))
        readout.pageDidFail(.server)
        readout.networkPathChanged(usable: true)
        next = readout.recovery
        XCTAssertEqual(next.pageFailed(.server), 60)
    }

    @MainActor
    func testEscapeNothingElseUsedClosesThePopover() async throws {
        _ = NSApplication.shared
        let controller = DashboardViewController(webView: RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration()))
        var escapes = 0
        controller.onEscape = { escapes += 1 }
        let above = KeyRecorder()
        controller.nextResponder = above
        // WebKit re-sends a key the page left unhandled up the responder chain.
        controller.keyDown(with: try key("\u{1b}", code: 53))
        XCTAssertEqual(escapes, 1)
        controller.keyDown(with: try key("a", code: 0))
        XCTAssertEqual(escapes, 1)
        XCTAssertEqual(above.keys, [0], "Other keys keep travelling up the responder chain")
        controller.cancelOperation(nil)
        XCTAssertEqual(escapes, 2)
    }

    @MainActor
    func testOpeningFocusesThePageOrTheStatusViewSoKeysArrive() async throws {
        _ = NSApplication.shared
        let location = try DashboardLocation("https://quota.example.com/app")
        let webView = RecordingWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let controller = DashboardViewController(webView: webView)
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 400, height: 620), styleMask: [.borderless],
                              backing: .buffered, defer: true)
        window.contentViewController = controller
        controller.configure(location)
        controller.prepareToShow()
        XCTAssertTrue(window.firstResponder === controller.view, "The loading view takes keys while the page is hidden")
        webView.pageURL = location.url
        controller.webView(webView, didFinish: nil)
        controller.prepareToShow()
        XCTAssertTrue(window.firstResponder === webView, "The page gets keys, and Escape, first")
        controller.webViewWebContentProcessDidTerminate(webView)
        XCTAssertTrue(window.firstResponder === controller.view)
    }
}

@MainActor
private final class KeyRecorder: NSResponder {
    var keys: [UInt16] = []
    override func keyDown(with event: NSEvent) { keys.append(event.keyCode) }
}

@MainActor
private func key(_ characters: String, code: UInt16) throws -> NSEvent {
    try XCTUnwrap(NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [], timestamp: 0, windowNumber: 0,
                                   context: nil, characters: characters, charactersIgnoringModifiers: characters,
                                   isARepeat: false, keyCode: code))
}
