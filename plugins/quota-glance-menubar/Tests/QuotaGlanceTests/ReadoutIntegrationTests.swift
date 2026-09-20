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
        let script = try XCTUnwrap(QuotaReadoutController.script(source, for: location, session: "integration"))
        let config = WKWebViewConfiguration()
        // A deterministic transport in a real WebKit page, with no visible
        // window and no page timer. The native call alone initiates the update.
        let transport = """
        window.testPercent = 43;
        window.fetch = async () => new Response(JSON.stringify({schemaVersion:1,stale:false,providers:[{
          id:'claude',title:'Claude',order:1,rows:[{rowId:'weekly_fable',title:'Weekly (Fable)',order:1,
          aggregate:{memberCount:1,remainingPercent:window.testPercent}}]}]}));
        """
        let receiver = ReadoutReceiver()
        config.userContentController.add(receiver, name: "quotaGlanceReadout")
        config.userContentController.addUserScript(WKUserScript(source: transport + "\n" + script, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        let webView = WKWebView(frame: .zero, configuration: config)
        let ready = NavigationReady()
        webView.navigationDelegate = ready
        webView.loadHTMLString("<html><body>Dashboard fixture</body></html>", baseURL: location.url)
        await fulfillment(of: [ready.finished], timeout: 20)
        XCTAssertNil(webView.window)
        _ = try await webView.callAsyncJavaScript("""
          await fetch('/proxy/v0/resource/plugins/quota-glance/summary');
          await window.__quotaGlanceReadout.refresh();
          window.testPercent = 18;
          await window.__quotaGlanceReadout.refresh();
          return true;
          """, arguments: [:], in: nil, in: .page)
        await fulfillment(of: [receiver.updated], timeout: 5)
    }
}

@MainActor
private final class NavigationReady: NSObject, WKNavigationDelegate {
    let finished = XCTestExpectation(description: "WebKit loaded the fixture")
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) { finished.fulfill() }
}

@MainActor
private final class ReadoutReceiver: NSObject, WKScriptMessageHandler {
    let updated = XCTestExpectation(description: "Hidden WebKit sent the updated quota")
    func userContentController(_ userContentController: WKUserContentController, didReceive message: WKScriptMessage) {
        guard let body = message.body as? [String: Any],
              let snapshot = body["snapshot"] as? [String: Any],
              let windows = snapshot["windows"] as? [[String: Any]],
              windows.first?["remainingPercent"] as? Int == 18 else { return }
        updated.fulfill()
    }
}
