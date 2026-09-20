import AppKit
import GlanceCore
import WebKit
import XCTest
@testable import QuotaGlance

final class ReadoutIntegrationTests: XCTestCase {
    func testSelectionPersistsAndChangingDashboardClearsIt() throws {
        let name = "QuotaGlanceTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let settings = AppSettings(defaults: defaults)
        let location = try DashboardLocation("https://quota.example.com/app")
        settings.save(location)
        let choice = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
        settings.quotaSelection = choice
        settings.save(location)
        XCTAssertEqual(AppSettings(defaults: defaults).quotaSelection, choice)
        settings.save(try DashboardLocation("https://other.example.com/app"))
        XCTAssertNil(settings.quotaSelection)
    }

    @MainActor
    func testNativeRefreshExecutesBridgeInAWebViewWithoutAWindow() async throws {
        _ = NSApplication.shared
        let directory = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let source = try String(contentsOf: directory.appendingPathComponent("Resources/QuotaReadout.js"))
        let location = try DashboardLocation("https://quota.example.com/proxy/v0/resource/plugins/quota-glance/app")
        let config = WKWebViewConfiguration()
        // A deterministic transport in a real WebKit page, with no visible
        // window and no page timer. The native call alone initiates the update.
        let transport = """
        window.testPercent = 43;
        window.fetch = async () => new Response(JSON.stringify({schemaVersion:1,stale:false,providers:[{
          id:'claude',title:'Claude',order:1,rows:[{rowId:'weekly_fable',title:'Weekly (Fable)',order:1,
          aggregate:{memberCount:1,remainingPercent:window.testPercent}}]}]}));
        """
        let webView = WKWebView(frame: .zero, configuration: config)
        let readout = QuotaReadoutController(webView: webView)
        defer { readout.stop() }
        readout.configure(location, scriptSource: transport + "\n" + source)
        let updated = XCTestExpectation(description: "Hidden WebKit updated the native quota state")
        let selection = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
        readout.onChange = {
            if readout.state.presentation(for: selection).text == "18%" { updated.fulfill() }
        }
        defer { readout.onChange = nil }
        let ready = NavigationReady()
        webView.navigationDelegate = ready
        webView.loadHTMLString("<html><body>Dashboard fixture</body></html>", baseURL: location.url)
        await fulfillment(of: [ready.finished], timeout: 20)
        XCTAssertNil(webView.window)
        _ = try await webView.callAsyncJavaScript("""
          await fetch('/proxy/v0/resource/plugins/quota-glance/summary');
          await window.__quotaGlanceReadout.refresh();
          window.testPercent = 18;
          return true;
          """, arguments: [:], in: nil, in: .page)
        readout.refresh()
        await fulfillment(of: [updated], timeout: 10)
    }
}

@MainActor
private final class NavigationReady: NSObject, WKNavigationDelegate {
    let finished = XCTestExpectation(description: "WebKit loaded the fixture")
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) { finished.fulfill() }
}
