import Foundation
import GlanceCore
import XCTest
@testable import QuotaGlance

final class AppSettingsMigrationTests: XCTestCase {
    private var suites: [String] = []

    private func defaults() -> UserDefaults {
        let name = "QuotaGlanceMigration.\(UUID().uuidString)"
        suites.append(name)
        return UserDefaults(suiteName: name)!
    }

    override func tearDown() {
        for name in suites { UserDefaults().removePersistentDomain(forName: name) }
        suites = []
        super.tearDown()
    }

    private let fable = QuotaSelection(providerID: "claude", rowID: "weekly_fable")

    func testPercentUserKeepsTheSameItemAndTheOldKey() throws {
        let store = defaults()
        store.set("https://quota.example.com/app", forKey: "dashboardURL")
        store.set(try JSONEncoder().encode(fable), forKey: "menuBarQuota")
        let settings = AppSettings(defaults: store)
        XCTAssertEqual(settings.readout, MenuBarReadout(style: .percent, windows: [fable], badge: .always, showsAppIcon: true))
        XCTAssertNotNil(store.data(forKey: "menuBarReadout"), "The migration is written back at once")
        XCTAssertNotNil(store.data(forKey: "menuBarQuota"), "0.3.1 still finds its key after a downgrade")
    }

    func testIconOnlyUserStaysIconOnly() {
        let store = defaults()
        store.set("https://quota.example.com/app", forKey: "dashboardURL")
        let settings = AppSettings(defaults: store)
        XCTAssertEqual(settings.readout.style, .iconOnly)
        XCTAssertEqual(settings.readout.windows, MenuBarReadout.defaultWindows)
        XCTAssertTrue(settings.readout.showsAppIcon)
    }

    func testNewInstallKeepsLetteredPairAfterSavingAURLAndRelaunching() throws {
        let store = defaults()
        let first = AppSettings(defaults: store)
        XCTAssertEqual(first.readout, .newInstall)
        first.save(try DashboardLocation("https://quota.example.com/app"))
        let relaunched = AppSettings(defaults: store)
        XCTAssertEqual(relaunched.readout.style, .letteredPair, "A saved URL must not turn a new install into Icon only")
        XCTAssertEqual(relaunched.readout, .newInstall)
    }

    func testStoredReadoutIsNotMigratedAgain() throws {
        let store = defaults()
        let chosen = MenuBarReadout(style: .splitPill, windows: [fable], badge: .whenProvidersDiffer, showsAppIcon: false)
        store.set(try JSONEncoder().encode(chosen), forKey: "menuBarReadout")
        store.set(try JSONEncoder().encode(QuotaSelection(providerID: "codex", rowID: "weekly")), forKey: "menuBarQuota")
        XCTAssertEqual(AppSettings(defaults: store).readout, chosen)
    }

    func testSavingFromSettingsOpenDuringAdoptionKeepsTheAdoptedWindows() throws {
        // A new install on a Codex-only dashboard, with Settings open when the
        // first summary arrives. Settings' draft must follow the adoption, or
        // Save would write the Claude pair back and adoption never runs again.
        let store = defaults()
        let settings = AppSettings(defaults: store)
        let location = try DashboardLocation("https://quota.example.com/app")
        settings.save(location)
        var draft = settings.readout
        draft.style = .splitPill
        let json = """
        {"stale":false,"windows":[
          {"selection":{"providerID":"codex","rowID":"session"},"title":"Codex · Session","remainingPercent":80},
          {"selection":{"providerID":"codex","rowID":"weekly"},"title":"Codex · Weekly","remainingPercent":60}]}
        """
        let codex = try JSONDecoder().decode(QuotaReadoutSnapshot.self, from: Data(json.utf8)).windows
        XCTAssertTrue(settings.adoptFirstSummary(codex))
        draft = draft.followingAdoptedWindows(settings.readout.windows)
        settings.save(location, readout: draft)
        XCTAssertEqual(settings.readout.windows, codex.map(\.selection))
        XCTAssertEqual(settings.readout.style, .splitPill)
        XCTAssertFalse(settings.adoptFirstSummary(codex), "Adoption runs once per dashboard")
        XCTAssertEqual(AppSettings(defaults: store).readout.windows, codex.map(\.selection))
    }

    func testFirstSummaryWithoutClaudeAdoptsItsFirstProviderOnce() throws {
        let store = defaults()
        let settings = AppSettings(defaults: store)
        settings.save(try DashboardLocation("https://quota.example.com/app"))
        let json = """
        {"stale":false,"windows":[
          {"selection":{"providerID":"codex","rowID":"session"},"title":"Codex · Session","remainingPercent":88},
          {"selection":{"providerID":"codex","rowID":"weekly"},"title":"Codex · Weekly","remainingPercent":66}]}
        """
        let snapshot = try JSONDecoder().decode(QuotaReadoutSnapshot.self, from: Data(json.utf8))
        XCTAssertTrue(settings.adoptFirstSummary(snapshot.windows))
        XCTAssertEqual(settings.readout.windows.map(\.providerID), ["codex", "codex"])
        // Later summaries never rewrite the choice, even back to the default pair.
        settings.readout = .newInstall
        XCTAssertFalse(settings.adoptFirstSummary(snapshot.windows))
        XCTAssertEqual(settings.readout, .newInstall)
    }
}
