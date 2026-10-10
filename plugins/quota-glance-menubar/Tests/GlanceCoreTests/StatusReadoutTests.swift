import Foundation
import XCTest
@testable import GlanceCore

final class StatusReadoutTests: XCTestCase {
    private let now = Date(timeIntervalSince1970: 1_000)
    private let session = QuotaSelection(providerID: "claude", rowID: "session")
    private let weekly = QuotaSelection(providerID: "claude", rowID: "weekly")
    private let codex = QuotaSelection(providerID: "codex", rowID: "weekly")

    private func state(session: Int = 94) throws -> QuotaReadoutState {
        let json = """
        {"stale":false,"windows":[
          {"selection":{"providerID":"claude","rowID":"session"},"title":"Claude · Session","remainingPercent":\(session)},
          {"selection":{"providerID":"claude","rowID":"weekly"},"title":"Claude · Weekly","remainingPercent":59},
          {"selection":{"providerID":"codex","rowID":"weekly"},"title":"Codex · Weekly","remainingPercent":36}]}
        """
        var state = QuotaReadoutState()
        state.receive(try JSONDecoder().decode(QuotaReadoutSnapshot.self, from: Data(json.utf8)), at: now)
        return state
    }

    private func readout(_ style: ReadoutStyle, badge: BadgeMode = .always) -> MenuBarReadout {
        MenuBarReadout(style: style, windows: [session, weekly, codex], badge: badge, showsAppIcon: false)
    }

    /// Claude has a bundled logo here; Codex doesn't.
    private func shown(_ readout: MenuBarReadout, _ state: QuotaReadoutState, dark: Bool = true, contrast: Bool = false,
                       at time: Date? = nil) -> StatusReadout {
        StatusReadout(readout: readout, state: state, dark: dark, increasedContrast: contrast,
                      hasLogo: { $0 == "claude" }, at: time ?? now)
    }

    func testIconOnlyAndPercentKeepTheSymbolAndTitle() throws {
        let icon = shown(readout(.iconOnly), try state())
        XCTAssertEqual(icon.length, .square)
        XCTAssertEqual(icon.image, .appIcon)
        XCTAssertEqual(icon.title, "")
        XCTAssertFalse(icon.imageLeading)
        XCTAssertEqual(icon.toolTip, "Quota Glance — right-click for settings")
        XCTAssertEqual(icon.accessibilityLabel, "Quota Glance. Quota Glance — right-click for settings")

        let percent = shown(readout(.percent), try state())
        XCTAssertEqual(percent.length, .variable)
        XCTAssertEqual(percent.image, .appIcon)
        XCTAssertEqual(percent.title, " 94%", "Percent shows the first window")
        XCTAssertTrue(percent.imageLeading)
        XCTAssertTrue(percent.toolTip.hasPrefix("Claude · Session: 94% remaining."))
        XCTAssertEqual(percent.accessibilityLabel, "Quota Glance. " + percent.toolTip)
    }

    func testDrawnStylesCarryEverythingTheImageDependsOn() throws {
        let current = try state()
        let pair = shown(readout(.letteredPair), current)
        let cells = current.cells(for: [session, weekly, codex], at: now)
        XCTAssertEqual(pair.length, .variable)
        XCTAssertEqual(pair.title, "")
        XCTAssertFalse(pair.imageLeading)
        XCTAssertEqual(pair.image, .drawn(cells: cells, options: ReadoutOptions(style: .letteredPair, badges: true, appIcon: false,
                                                                                  dark: true, increasedContrast: false),
                                          logos: ["claude"]))
        XCTAssertEqual(pair.toolTip, ReadoutText.tooltip(cells))
        XCTAssertEqual(pair.accessibilityLabel, ReadoutText.accessibilityLabel(cells))
        // Badges off draw no logos, so none are part of the image.
        guard case let .drawn(_, _, logos) = shown(readout(.splitPill, badge: .off), current).image else {
            return XCTFail("Split pill is drawn")
        }
        XCTAssertEqual(logos, [])
    }

    func testTheSameReadoutChangesNothing() throws {
        // The rule that breaks 0.4.0's loop: an appearance notification that
        // leaves the settled appearance as it was must write nothing.
        for style in ReadoutStyle.allCases {
            let first = shown(readout(style), try state())
            XCTAssertEqual(first.changes(from: shown(readout(style), try state())), [], "\(style)")
            XCTAssertEqual(first.changes(from: nil), .all, "\(style): the first render writes everything")
        }
    }

    func testAppearanceAndContrastRedrawOnlyTheDrawnImage() throws {
        let current = try state()
        for style in [ReadoutStyle.letteredPair, .splitPill] {
            let dark = shown(readout(style), current)
            XCTAssertEqual(shown(readout(style), current, dark: false).changes(from: dark), .image, "\(style)")
            XCTAssertEqual(shown(readout(style), current, contrast: true).changes(from: dark), .image, "\(style)")
        }
        // The symbol is a template image the system tints: nothing to redraw.
        for style in [ReadoutStyle.iconOnly, .percent] {
            let dark = shown(readout(style), current)
            XCTAssertEqual(shown(readout(style), current, dark: false, contrast: true).changes(from: dark), [], "\(style)")
        }
    }

    func testNewReadingsWriteOnlyWhatTheyChange() throws {
        let before = try state(), after = try state(session: 93)
        XCTAssertEqual(shown(readout(.percent), after).changes(from: shown(readout(.percent), before)),
                       [.title, .toolTip, .accessibilityLabel])
        XCTAssertEqual(shown(readout(.letteredPair), after).changes(from: shown(readout(.letteredPair), before)),
                       [.image, .toolTip, .accessibilityLabel])
        XCTAssertEqual(shown(readout(.iconOnly), after).changes(from: shown(readout(.iconOnly), before)), [])
    }

    func testExpiryStillRedraws() throws {
        // The minute timer re-renders with the same state; a reading older
        // than 150 seconds must still turn into "—".
        let current = try state()
        for style in ReadoutStyle.allCases where style != .iconOnly {
            let fresh = shown(readout(style), current)
            let expired = shown(readout(style), current, at: now.addingTimeInterval(151))
            XCTAssertFalse(expired.changes(from: fresh).isEmpty, "\(style)")
        }
    }

    func testSwitchingStylesWritesTheLengthOnlyWhenItChanges() throws {
        let current = try state()
        let icon = shown(readout(.iconOnly), current)
        let percent = shown(readout(.percent), current)
        let pair = shown(readout(.letteredPair), current)
        XCTAssertEqual(percent.changes(from: icon), [.length, .title, .imagePosition, .toolTip, .accessibilityLabel])
        XCTAssertEqual(pair.changes(from: percent), [.image, .title, .imagePosition, .toolTip, .accessibilityLabel])
        XCTAssertEqual(icon.changes(from: pair), [.length, .image, .toolTip, .accessibilityLabel],
                       "Both show the image alone with no title")
    }
}
