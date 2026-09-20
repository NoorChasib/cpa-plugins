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
    override var url: URL? { pageURL }
    override var isLoading: Bool { stubLoading }
    override func load(_ request: URLRequest) -> WKNavigation? {
        loads += 1
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
}
