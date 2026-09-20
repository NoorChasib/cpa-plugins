import Foundation
import XCTest
@testable import GlanceCore

final class DashboardLocationTests: XCTestCase {
    func testKeepsReverseProxyPrefixAndNonCredentialQuery() throws {
        let location = try DashboardLocation("  https://quota.example.com/cpa/v0/resource/plugins/quota-glance/app?view=compact\n")
        XCTAssertEqual(location.url.absoluteString, "https://quota.example.com/cpa/v0/resource/plugins/quota-glance/app?view=compact")
    }

    func testAcceptsLocalNetworkAndDevelopmentServers() throws {
        for value in ["http://100.64.0.1:8317/v0/resource/plugins/quota-glance/app", "http://localhost:5173/", "http://[::1]:5173/"] {
            XCTAssertNoThrow(try DashboardLocation(value), value)
        }
    }

    func testRejectsInvalidDestinations() {
        for value in ["", "example.com/app", "/app", "file:///tmp/app.html", "javascript:alert(1)", "https:///", "https://exa mple.com", "https://example.com:0", "https://example.com:65536"] {
            XCTAssertThrowsError(try DashboardLocation(value), value)
        }
    }

    func testCredentialsAreNotStoredInThePreferenceURL() {
        for value in [
            "https://user:secret@example.com/app", "https://user@example.com/app",
            "https://example.com/app?token=secret", "https://example.com/app?TOKEN=secret",
            "https://example.com/app?%74oken=secret", "https://example.com/app?access_token=secret",
            "https://example.com/app?password=secret", "https://example.com/app?managementKey=secret",
        ] {
            XCTAssertThrowsError(try DashboardLocation(value), value) { error in
                XCTAssertEqual(error as? DashboardLocation.ValidationError, .embeddedCredential)
            }
        }
    }

    func testConsoleAuthenticationStaysInTheEmbeddedBrowser() throws {
        let location = try DashboardLocation("https://quota.example.com/cpa/v0/resource/plugins/quota-glance/app")
        for value in ["https://quota.example.com/cpa/", "https://QUOTA.example.com:443/login", "https://quota.example.com/cpa/management.html"] {
            XCTAssertEqual(location.navigation(to: try XCTUnwrap(URL(string: value)), userActivatedLink: true), .embedded)
        }
    }

    func testExternalUserClicksOpenInBrowserButRedirectsCannotSilentlyReplaceDashboard() throws {
        let location = try DashboardLocation("https://quota.example.com/app")
        for value in ["https://docs.example.com/", "http://quota.example.com/app", "https://quota.example.com:444/app", "https://quota.example.com.evil.example/"] {
            let url = try XCTUnwrap(URL(string: value))
            XCTAssertEqual(location.navigation(to: url, userActivatedLink: true), .browser)
            XCTAssertEqual(location.navigation(to: url, userActivatedLink: false), .blocked)
        }
    }

    func testNeverOpensExecutableSchemesOrURLsWithUserInfo() throws {
        let location = try DashboardLocation("https://quota.example.com/app")
        for value in ["javascript:alert(1)", "file:///tmp/a", "data:text/html,hello", "custom-app://open", "https://user:secret@quota.example.com/app"] {
            XCTAssertEqual(location.navigation(to: try XCTUnwrap(URL(string: value)), userActivatedLink: true), .blocked)
        }
    }

    func testDashboardReloadDoesNotReplaceConsoleNavigation() throws {
        let location = try DashboardLocation("https://quota.example.com/cpa/app?view=compact")
        XCTAssertTrue(location.isDashboard(try XCTUnwrap(URL(string: "https://quota.example.com:443/cpa/app?view=compact#weekly"))))
        for value in ["https://quota.example.com/cpa/", "https://quota.example.com/cpa/app?view=other", "https://other.example.com/cpa/app?view=compact"] {
            XCTAssertFalse(location.isDashboard(try XCTUnwrap(URL(string: value))))
        }
    }
}
