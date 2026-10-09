import Foundation
import XCTest
@testable import GlanceCore

final class MenuBarReadoutTests: XCTestCase {
    private let fable = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
    private let codexWeekly = QuotaSelection(providerID: "codex", rowID: "weekly")

    private func encode<T: Encodable>(_ value: T) throws -> Data { try JSONEncoder().encode(value) }

    func testStoredReadoutWinsOverEverything() throws {
        let stored = MenuBarReadout(style: .splitPill, windows: [codexWeekly], badge: .off, showsAppIcon: true)
        let result = MenuBarReadout.resolve(stored: try encode(stored), legacySelection: try encode(fable), hasDashboardURL: true)
        XCTAssertEqual(result.readout, stored)
        XCTAssertFalse(result.migrated)
    }

    func testLegacySelectionBecomesPercentWithThatWindow() throws {
        let result = MenuBarReadout.resolve(stored: nil, legacySelection: try encode(fable), hasDashboardURL: true)
        XCTAssertEqual(result.readout, MenuBarReadout(style: .percent, windows: [fable], badge: .always, showsAppIcon: true))
        XCTAssertTrue(result.migrated)
    }

    func testSavedURLWithoutSelectionStaysIconOnlyWithTheDefaultPairReady() {
        let result = MenuBarReadout.resolve(stored: nil, legacySelection: nil, hasDashboardURL: true)
        XCTAssertEqual(result.readout.style, .iconOnly)
        XCTAssertEqual(result.readout.windows, MenuBarReadout.defaultWindows)
        XCTAssertEqual(result.readout.badge, .always)
        XCTAssertTrue(result.readout.showsAppIcon)
        XCTAssertTrue(result.migrated)
    }

    func testNewInstallStartsWithLetteredPairAndNoIcon() {
        let result = MenuBarReadout.resolve(stored: nil, legacySelection: nil, hasDashboardURL: false)
        XCTAssertEqual(result.readout, MenuBarReadout(style: .letteredPair, windows: MenuBarReadout.defaultWindows, badge: .always, showsAppIcon: false))
        XCTAssertTrue(result.migrated)
    }

    func testGarbageStoredValueFallsThroughToMigration() throws {
        let result = MenuBarReadout.resolve(stored: Data("not json".utf8), legacySelection: try encode(fable), hasDashboardURL: true)
        XCTAssertEqual(result.readout.style, .percent)
        XCTAssertTrue(result.migrated)
        let bad = MenuBarReadout.resolve(stored: nil, legacySelection: Data("{}".utf8), hasDashboardURL: true)
        XCTAssertEqual(bad.readout.style, .iconOnly, "An unreadable old selection is Icon only, as 0.3.1 showed it")
    }

    func testUnknownValuesDecodeToDefaults() throws {
        let json = #"{"style":"sparkline","badge":"sometimes","showsAppIcon":"yes","windows":[{"providerID":"claude","rowID":"weekly"},7,{"providerID":""},{"providerID":"codex","rowID":"weekly"}],"extra":1}"#
        let value = try JSONDecoder().decode(MenuBarReadout.self, from: Data(json.utf8))
        XCTAssertEqual(value.style, .letteredPair)
        XCTAssertEqual(value.badge, .always)
        XCTAssertFalse(value.showsAppIcon)
        XCTAssertEqual(value.windows, [QuotaSelection(providerID: "claude", rowID: "weekly"), codexWeekly])
        let empty = try JSONDecoder().decode(MenuBarReadout.self, from: Data("{}".utf8))
        XCTAssertEqual(empty, MenuBarReadout.newInstall)
    }

    func testRoundTripUsesTheDocumentedKeys() throws {
        let value = MenuBarReadout(style: .splitPill, windows: [fable, codexWeekly], badge: .whenProvidersDiffer, showsAppIcon: true)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: try encode(value)) as? [String: Any])
        XCTAssertEqual(Set(object.keys), ["style", "windows", "badge", "showsAppIcon"])
        XCTAssertEqual(object["style"] as? String, "splitPill")
        XCTAssertEqual(object["badge"] as? String, "whenProvidersDiffer")
        XCTAssertEqual(try JSONDecoder().decode(MenuBarReadout.self, from: try encode(value)), value)
    }

    func testWindowsAreDedupedClampedToThreeAndNeverEmpty() {
        let a = QuotaSelection(providerID: "claude", rowID: "session")
        let b = QuotaSelection(providerID: "claude", rowID: "weekly")
        let many = MenuBarReadout(style: .splitPill, windows: [a, b, a, fable, codexWeekly], badge: .always, showsAppIcon: false)
        XCTAssertEqual(many.windows, [a, b, fable])
        let none = MenuBarReadout(style: .splitPill, windows: [], badge: .always, showsAppIcon: false)
        XCTAssertEqual(none.windows, MenuBarReadout.defaultWindows)
    }

    func testNewDashboardResetsWindowsAndKeepsStyle() {
        let value = MenuBarReadout(style: .splitPill, windows: [codexWeekly], badge: .off, showsAppIcon: true).resettingWindows()
        XCTAssertEqual(value, MenuBarReadout(style: .splitPill, windows: MenuBarReadout.defaultWindows, badge: .off, showsAppIcon: true))
    }

    private func window(_ provider: String, _ row: String) -> QuotaWindow {
        let json = #"{"selection":{"providerID":"\#(provider)","rowID":"\#(row)"},"title":"\#(provider) · \#(row)","remainingPercent":50}"#
        return try! JSONDecoder().decode(QuotaWindow.self, from: Data(json.utf8))
    }

    func testFirstSummaryWithoutClaudeSwapsOnlyTheUntouchedDefaultPair() {
        let codexOnly = [window("codex", "session"), window("codex", "weekly"), window("xai", "credits")]
        let swapped = MenuBarReadout.newInstall.adoptingFirstSummary(codexOnly)
        XCTAssertEqual(swapped?.windows, [QuotaSelection(providerID: "codex", rowID: "session"), codexWeekly])
        XCTAssertEqual(swapped?.style, .letteredPair)
        let xaiOnly = MenuBarReadout.newInstall.adoptingFirstSummary([window("xai", "credits")])
        XCTAssertEqual(xaiOnly?.windows, [QuotaSelection(providerID: "xai", rowID: "credits")])
        XCTAssertNil(MenuBarReadout.newInstall.adoptingFirstSummary([window("claude", "weekly"), window("codex", "weekly")]))
        XCTAssertNil(MenuBarReadout.newInstall.adoptingFirstSummary([]))
        let chosen = MenuBarReadout(style: .letteredPair, windows: [fable], badge: .always, showsAppIcon: false)
        XCTAssertNil(chosen.adoptingFirstSummary(codexOnly))
    }

    func testBadgeModes() {
        let claude = MenuBarReadout(style: .letteredPair, windows: MenuBarReadout.defaultWindows, badge: .whenProvidersDiffer, showsAppIcon: false)
        XCTAssertFalse(claude.badgeVisible)
        var mixed = claude
        mixed.windows = [codexWeekly, fable]
        XCTAssertTrue(mixed.badgeVisible)
        mixed.badge = .off
        XCTAssertFalse(mixed.badgeVisible)
        mixed.badge = .always
        XCTAssertTrue(mixed.badgeVisible)
        XCTAssertTrue(BadgeMode.always.isVisible(forProviders: ["claude"]))
    }
}
