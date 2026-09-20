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
}
