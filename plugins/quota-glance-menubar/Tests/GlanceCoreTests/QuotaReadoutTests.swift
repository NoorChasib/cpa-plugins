import Foundation
import XCTest
@testable import GlanceCore

final class QuotaReadoutTests: XCTestCase {
    private let selection = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
    private let now = Date(timeIntervalSince1970: 1_000)

    private func snapshot(percent: String = "18", stale: Bool = false) throws -> QuotaReadoutSnapshot {
        let json = """
        {"stale":\(stale),"windows":[{"selection":{"providerID":"claude","rowID":"weekly_fable"},"title":"Claude · Weekly (Fable)","remainingPercent":\(percent)}]}
        """
        return try JSONDecoder().decode(QuotaReadoutSnapshot.self, from: Data(json.utf8))
    }

    func testUsesSelectedAggregateAndUpdatesItsPercentage() throws {
        var state = QuotaReadoutState()
        state.receive(try snapshot(), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "18%")
        state.receive(try snapshot(percent: "43"), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "43%")
        XCTAssertTrue(state.presentation(for: selection, at: now).detail.contains("Weekly (Fable)"))
    }

    func testUnknownMissingAndExpiredReadingsNeverLookCurrent() throws {
        var state = QuotaReadoutState()
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "—%")
        state.receive(try snapshot(), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now.addingTimeInterval(151)).text, "—%")
        XCTAssertEqual(state.presentation(for: .init(providerID: "codex", rowID: "weekly"), at: now).text, "—%")
        state.markUnavailable()
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "—%")
        XCTAssertEqual(state.windows.count, 1, "A failure must not remove the saved quota's picker option")
    }

    func testNoReadingAndStaleAreDifferentFromZeroRemaining() throws {
        var state = QuotaReadoutState()
        state.receive(try snapshot(percent: "0"), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "0%")
        state.receive(try snapshot(percent: "null"), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "—%")
        state.receive(try snapshot(stale: true), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "—%")
    }

    func testInvalidPercentIsRejectedAndIconOnlyHasNoTitle() throws {
        var state = QuotaReadoutState()
        state.receive(try snapshot(percent: "101"), at: now)
        XCTAssertEqual(state.presentation(for: selection, at: now).text, "—%")
        XCTAssertEqual(state.presentation(for: nil, at: now).text, "")
    }

    func testSelectionPersistsBothProviderAndWindowIdentity() throws {
        let data = try JSONEncoder().encode(selection)
        XCTAssertEqual(try JSONDecoder().decode(QuotaSelection.self, from: data), selection)
        XCTAssertNotEqual(selection, QuotaSelection(providerID: "codex", rowID: "weekly_fable"))
    }

    private func summary(stale: Bool = false, session: String = "94") throws -> QuotaReadoutSnapshot {
        let json = """
        {"stale":\(stale),"windows":[
          {"selection":{"providerID":"claude","rowID":"session"},"title":"Claude · Session","remainingPercent":\(session)},
          {"selection":{"providerID":"claude","rowID":"weekly"},"title":"Claude · Weekly","remainingPercent":0},
          {"selection":{"providerID":"codex","rowID":"weekly"},"title":"Codex · Weekly","remainingPercent":66}]}
        """
        return try JSONDecoder().decode(QuotaReadoutSnapshot.self, from: Data(json.utf8))
    }

    func testCellsCarryLetterProviderAndPercentInOrder() throws {
        var state = QuotaReadoutState()
        state.receive(try summary(), at: now)
        let chosen = [QuotaSelection(providerID: "codex", rowID: "weekly"), QuotaSelection(providerID: "claude", rowID: "session"),
                      QuotaSelection(providerID: "claude", rowID: "weekly")]
        let cells = state.cells(for: chosen, at: now)
        XCTAssertEqual(cells.map(\.letter), ["W", "S", "W"])
        XCTAssertEqual(cells.map(\.selection.providerID), ["codex", "claude", "claude"])
        XCTAssertEqual(cells.map(\.text), ["66", "94", "0"], "0 is a real reading, not a dash")
        XCTAssertTrue(cells.allSatisfy(\.available))
    }

    func testStaleMissingExpiredAndGoneCellsShowADash() throws {
        let session = QuotaSelection(providerID: "claude", rowID: "session")
        let gone = QuotaSelection(providerID: "claude", rowID: "weekly_fable")
        var state = QuotaReadoutState()
        XCTAssertEqual(state.cells(for: [session], at: now).map(\.text), ["—"])
        state.receive(try summary(session: "null"), at: now)
        XCTAssertEqual(state.cells(for: [session], at: now).map(\.text), ["—"])
        state.receive(try summary(), at: now)
        XCTAssertEqual(state.cells(for: [session], at: now.addingTimeInterval(151)).map(\.text), ["—"])
        XCTAssertEqual(state.cells(for: [session], at: now.addingTimeInterval(150)).map(\.text), ["94"])
        state.receive(try summary(stale: true), at: now)
        XCTAssertEqual(state.cells(for: [session], at: now).map(\.text), ["—"])
        state.receive(try summary(), at: now)
        let cell = try XCTUnwrap(state.cells(for: [gone], at: now).first)
        XCTAssertEqual(cell.text, "—")
        XCTAssertEqual(cell.letter, "F")
        XCTAssertFalse(cell.available)
        XCTAssertEqual(cell.title, "Claude · weekly_fable")
        state.markUnavailable()
        XCTAssertEqual(state.cells(for: [session], at: now).map(\.text), ["—"])
    }

    func testAccessibilityAndTooltipText() throws {
        var state = QuotaReadoutState()
        state.receive(try summary(session: "null"), at: now)
        let cells = state.cells(for: [QuotaSelection(providerID: "claude", rowID: "session"), QuotaSelection(providerID: "codex", rowID: "weekly")], at: now)
        XCTAssertEqual(ReadoutText.accessibilityLabel(cells),
                       "Quota Glance. Claude session, no current reading, Codex weekly 66 percent remaining.")
        XCTAssertEqual(ReadoutText.tooltip(cells), """
            Claude · Session: no current reading.
            Codex · Weekly: 66% remaining.
            Open the dashboard to check its connection and sign-in.
            Updates every minute while your Mac is awake.
            """)
        state.receive(try summary(), at: now)
        let fresh = state.cells(for: [QuotaSelection(providerID: "claude", rowID: "session")], at: now)
        XCTAssertEqual(ReadoutText.accessibilityLabel(fresh), "Quota Glance. Claude session 94 percent remaining.")
        XCTAssertEqual(ReadoutText.tooltip(fresh), "Claude · Session: 94% remaining.\nUpdates every minute while your Mac is awake.")
    }
}
