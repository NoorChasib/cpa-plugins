import AppKit
import GlanceCore
import WebKit
import XCTest
@testable import QuotaGlance

final class ReadoutIntegrationTests: XCTestCase {
    func testSelectionPersistsAndChangingDashboardResetsItsWindows() throws {
        let name = "QuotaGlanceTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: name)!
        defer { defaults.removePersistentDomain(forName: name) }
        let settings = AppSettings(defaults: defaults)
        let location = try DashboardLocation("https://quota.example.com/app")
        settings.save(location)
        let fable = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
        let choice = MenuBarReadout(style: .splitPill, windows: [fable], badge: .off, showsAppIcon: true)
        settings.save(location, readout: choice)
        XCTAssertEqual(AppSettings(defaults: defaults).readout, choice)
        // A different dashboard resets the windows to the default pair and keeps the style.
        settings.save(try DashboardLocation("https://other.example.com/app"), readout: choice)
        XCTAssertEqual(settings.readout, MenuBarReadout(style: .splitPill, windows: MenuBarReadout.defaultWindows, badge: .off, showsAppIcon: true))
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
          """, arguments: [:], in: nil, contentWorld: .page)
        readout.refresh()
        await fulfillment(of: [updated], timeout: 10)
    }

    // CPA counts a refused management key toward its ban, and a console read
    // that got no answer may have been counted: the native clock never repeats it.
    @MainActor
    func testConsoleReadWithoutAnAnswerIsNeverRepeated() async throws {
        _ = NSApplication.shared
        let directory = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let source = try String(contentsOf: directory.appendingPathComponent("Resources/QuotaReadout.js"))
        let location = try DashboardLocation("https://quota.example.com/proxy/v0/resource/plugins/quota-glance/app")
        // The server answers the reachability check (CPA 404s a HEAD for the
        // page), so the console read itself is what goes unanswered.
        let transport = """
        window.reads = 0;
        window.heads = 0;
        window.fetch = async (input, init) => {
          if ((init?.method ?? input?.method ?? 'GET') === 'HEAD') {
            window.heads += 1;
            return new Response(null, {status: 404});
          }
          window.reads += 1;
          if (window.offline) throw new TypeError('Load failed');
          return new Response(JSON.stringify({schemaVersion:1,stale:false,providers:[{
            id:'claude',title:'Claude',order:1,rows:[{rowId:'weekly_fable',title:'Weekly (Fable)',order:1,
            aggregate:{memberCount:1,remainingPercent:43}}]}]}));
        };
        """
        let webView = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let readout = QuotaReadoutController(webView: webView)
        defer { readout.stop() }
        readout.configure(location, scriptSource: transport + "\n" + source)
        let ready = NavigationReady()
        webView.navigationDelegate = ready
        webView.loadHTMLString("<html><body>Dashboard fixture</body></html>", baseURL: location.url)
        await fulfillment(of: [ready.finished], timeout: 20)
        let reads = try await webView.callAsyncJavaScript("""
          await fetch('/proxy/v0/management/plugins/quota-glance/summary');
          await window.__quotaGlanceReadout.refresh();
          window.offline = true;
          await window.__quotaGlanceReadout.refresh();
          window.offline = false;
          await window.__quotaGlanceReadout.refresh();
          await window.__quotaGlanceReadout.refresh();
          return [window.reads, window.heads];
          """, arguments: [:], in: nil, contentWorld: .page)
        XCTAssertEqual(reads as? [Int], [3, 2],
                       "The page's read, a replay, and the replay with no answer, each replay checked first; nothing after it")
    }

    // The app asks for a reading as soon as the Mac wakes, usually before
    // Wi-Fi or a VPN is back. The console replay used to go out then, get no
    // answer and be dropped, leaving the readout dark until the popover opened.
    // Here the server cannot be reached for the first replay, then can.
    @MainActor
    func testConsoleReplayWaitsUntilTheServerAnswers() async throws {
        _ = NSApplication.shared
        let directory = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let source = try String(contentsOf: directory.appendingPathComponent("Resources/QuotaReadout.js"))
        let location = try DashboardLocation("https://quota.example.com/proxy/v0/resource/plugins/quota-glance/app")
        let transport = """
        window.reads = 0;
        window.heads = 0;
        window.testPercent = 43;
        window.fetch = async (input, init) => {
          if (window.offline) throw new TypeError('Load failed');
          if ((init?.method ?? input?.method ?? 'GET') === 'HEAD') {
            window.heads += 1;
            return new Response(null, {status: 404});
          }
          window.reads += 1;
          return new Response(JSON.stringify({schemaVersion:1,stale:false,providers:[{
            id:'claude',title:'Claude',order:1,rows:[{rowId:'weekly_fable',title:'Weekly (Fable)',order:1,
            aggregate:{memberCount:1,remainingPercent:window.testPercent}}]}]}));
        };
        """
        let webView = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        let readout = QuotaReadoutController(webView: webView)
        defer { readout.stop() }
        readout.configure(location, scriptSource: transport + "\n" + source)
        let selection = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
        let retrying = XCTestExpectation(description: "No answer asked for a quick poll retry")
        retrying.assertForOverFulfill = false
        let updated = XCTestExpectation(description: "The replay read through the console once the server answered")
        updated.assertForOverFulfill = false
        readout.onChange = {
            if readout.recovery.pending == .poll { retrying.fulfill() }
            if readout.state.presentation(for: selection).text == "18%" { updated.fulfill() }
        }
        defer { readout.onChange = nil }
        let ready = NavigationReady()
        webView.navigationDelegate = ready
        webView.loadHTMLString("<html><body>Dashboard fixture</body></html>", baseURL: location.url)
        await fulfillment(of: [ready.finished], timeout: 20)
        let counts = try await webView.callAsyncJavaScript("""
          await fetch('/proxy/v0/management/plugins/quota-glance/summary');
          window.offline = true;
          await window.__quotaGlanceReadout.refresh();
          window.offline = false;
          window.testPercent = 18;
          await window.__quotaGlanceReadout.refresh();
          return [window.reads, window.heads];
          """, arguments: [:], in: nil, contentWorld: .page)
        XCTAssertEqual(counts as? [Int], [2, 1], "Nothing reached the console while the server could not be reached")
        await fulfillment(of: [retrying, updated], timeout: 10, enforceOrder: true)
    }
}

@MainActor
private final class NavigationReady: NSObject, WKNavigationDelegate {
    let finished = XCTestExpectation(description: "WebKit loaded the fixture")
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) { finished.fulfill() }
}
