import Foundation
import XCTest
@testable import GlanceCore

final class ReadoutLettersTests: XCTestCase {
    private struct Fixture: Decodable {
        struct Scenario: Decodable { let name: String; let rows: [Row] }
        struct Row: Decodable { let providerID: String; let rowID: String; let short: String; let long: String; let letter: String }
        let scenarios: [Scenario]
    }

    private func sel(_ provider: String, _ row: String) -> QuotaSelection { QuotaSelection(providerID: provider, rowID: row) }

    func testMatchesTheBoardGeneratedFixture() throws {
        let url = URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("Fixtures/letters.json")
        let fixture = try JSONDecoder().decode(Fixture.self, from: Data(contentsOf: url))
        XCTAssertGreaterThanOrEqual(fixture.scenarios.count, 5)
        for scenario in fixture.scenarios {
            let rows = scenario.rows.map { sel($0.providerID, $0.rowID) }
            for row in scenario.rows {
                let forms = ReadoutLetters.forms(rowID: row.rowID)
                XCTAssertEqual(forms.short, row.short, "\(scenario.name): \(row.rowID) short")
                XCTAssertEqual(forms.long, row.long, "\(scenario.name): \(row.rowID) long")
                XCTAssertEqual(ReadoutLetters.letter(for: sel(row.providerID, row.rowID), among: rows), row.letter, "\(scenario.name): \(row.rowID)")
            }
        }
    }

    func testEveryRowShape() {
        let rows = [sel("claude", "session"), sel("claude", "weekly"), sel("claude", "weekly_fable"), sel("claude", "model_weekly:sonnet"),
                    sel("codex", "model_session:spark"), sel("codex", "model_weekly:spark"), sel("xai", "credits"), sel("xai", "raw:xai:product/BUILD")]
        XCTAssertEqual(rows.map { ReadoutLetters.letter(for: $0, among: rows) }, ["S", "W", "F", "Ws", "Ss", "Ws", "C", "P"])
        XCTAssertEqual(ReadoutLetters.forms(rowID: "monthly").short, "M")
        XCTAssertEqual(ReadoutLetters.forms(rowID: "raw:xai:sandbox").short, "Sa")
        XCTAssertEqual(ReadoutLetters.forms(rowID: "raw:xai:Omega").short, "Om")
        XCTAssertEqual(ReadoutLetters.forms(rowID: "model_weekly:llama").short, "Wa")
    }

    func testShapesTheBoardCannotDrawStillGetALetter() {
        XCTAssertEqual(ReadoutLetters.forms(rowID: "daily").short, "D")
        XCTAssertEqual(ReadoutLetters.forms(rowID: "raw:xai:2026").short, "?")
        XCTAssertEqual(ReadoutLetters.forms(rowID: "model_weekly:4").short, "W")
    }

    func testLetterDoesNotChangeWithTheSelection() {
        let rows = [sel("codex", "session"), sel("codex", "weekly"), sel("codex", "model_weekly:spark")]
        let spark = sel("codex", "model_weekly:spark")
        let alone = ReadoutLetters.letter(for: spark, among: rows)
        // Whatever else is chosen, or in which order, the letter is computed from the summary's rows.
        XCTAssertEqual(alone, "Ws")
        XCTAssertEqual(ReadoutLetters.letter(for: spark, among: rows.reversed()), "Ws")
    }

    func testANewRowWithASharedInitialChangesTheLetterAndTheClashReFires() {
        let spark = sel("codex", "model_weekly:spark"), sage = sel("codex", "model_weekly:sage")
        let before = [sel("codex", "weekly"), spark]
        XCTAssertEqual(ReadoutLetters.letter(for: spark, among: before), "Ws")
        let after = before + [sage]
        XCTAssertEqual(ReadoutLetters.letter(for: spark, among: after), "Wsp")
        XCTAssertEqual(ReadoutLetters.letter(for: sage, among: after), "Wsa")
        // A chosen window missing from the summary still gets a letter.
        XCTAssertEqual(ReadoutLetters.letter(for: sage, among: before), "Wsa", "An absent window is still compared with the summary's rows")
        // Two bulk/BUILD raw rows stay alike after the long form: Settings warns.
        let raw = [sel("xai", "raw:xai:BUILD"), sel("xai", "raw:xai:bulk")]
        let letters = raw.map { ReadoutLetters.letter(for: $0, among: raw) }
        XCTAssertEqual(letters, ["Bu", "Bu"])
        XCTAssertNotNil(ReadoutLetters.clash(raw, letters: letters, badgeVisible: true))
    }

    func testClashRules() {
        let windows = [sel("codex", "weekly"), sel("claude", "weekly")]
        let letters = ["W", "W"]
        XCTAssertNil(ReadoutLetters.clash(windows, letters: letters, badgeVisible: true), "Badges tell providers apart")
        let off = ReadoutLetters.clash(windows, letters: letters, badgeVisible: false)
        XCTAssertEqual(off?.first, 0)
        XCTAssertEqual(off?.second, 1)
        XCTAssertEqual(off?.sameProvider, false)
        XCTAssertNil(ReadoutLetters.clash([sel("claude", "session"), sel("claude", "weekly")], letters: ["S", "W"], badgeVisible: false))
    }
}
