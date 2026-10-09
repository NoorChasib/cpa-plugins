import Foundation
import XCTest
@testable import GlanceCore

final class ReadoutLayoutTests: XCTestCase {
    private func run(letter: Double = 8, number: Double = 15, lsb: Double = 0.6, rsb: Double = 0.5, badge: Double? = 0.2) -> ReadoutRun {
        ReadoutRun(letterAdvance: letter, letterLeftBearing: lsb, numberAdvance: number, numberRightBearing: rsb, badgeRightInset: badge)
    }

    func testPairSlotHoldsTwoDigitsAndOnlyAHundredWidensIt() {
        let two = ReadoutLayout.pairSlotWidth(letterAdvance: 7.3, numberAdvance: 14.2, twoDigitAdvance: 14.2, badge: true)
        let one = ReadoutLayout.pairSlotWidth(letterAdvance: 7.3, numberAdvance: 7.1, twoDigitAdvance: 14.2, badge: true)
        let dash = ReadoutLayout.pairSlotWidth(letterAdvance: 7.3, numberAdvance: 9.0, twoDigitAdvance: 14.2, badge: true)
        let hundred = ReadoutLayout.pairSlotWidth(letterAdvance: 7.3, numberAdvance: 21.3, twoDigitAdvance: 14.2, badge: true)
        XCTAssertEqual(two, 33)          // 7.3 + 1 + 14.2 + 1 + 9 = 32.5, rounded up to a whole point
        XCTAssertEqual(one, two)
        XCTAssertEqual(dash, two)
        XCTAssertEqual(hundred, 40)
        XCTAssertEqual(ReadoutLayout.pairSlotWidth(letterAdvance: 7.3, numberAdvance: 14.2, twoDigitAdvance: 14.2, badge: false), 23)
        XCTAssertEqual(ReadoutLayout.pairGap(badges: true), 6)
        XCTAssertEqual(ReadoutLayout.pairGap(badges: false), 7)
    }

    func testPillHalvesAreEqualAndSizedByTheWidestTwoDigitInkPlusPadding() {
        let s = run(letter: 7.6), w = run(letter: 11.4)
        // W's ink: 11.4 + 1 + 15 + 10 - 0.6 - 0.2 = 36.6; plus 5.5 each side = 47.6 → 48.
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s, w]), 48)
        XCTAssertEqual(ReadoutLayout.pillWidth(halfWidth: 48, halves: 2), 97)
        XCTAssertEqual(ReadoutLayout.pillWidth(halfWidth: 48, halves: 3), 146)
        XCTAssertEqual(ReadoutLayout.pillWidth(halfWidth: 48, halves: 1), 48)
    }

    func testAHundredFitsThePaddingWithoutChangingWidth() {
        let s = run(letter: 7.6), w = run(letter: 11.4)
        let s100 = run(letter: 7.6, number: 22.5), w100 = run(letter: 11.4, number: 22.5)
        let normal = ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s, w])
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s100, w]), normal)
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s100, w100]), normal)
        // A reading that cannot keep a point of room grows every half alike.
        let huge = run(letter: 11.4, number: 40)
        XCTAssertGreaterThan(ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s, huge]), normal)
    }

    func testContentIsCentredByInkOnThePixelGrid() {
        let r = run(letter: 7.6)
        let left = ReadoutLayout.pillContentOrigin(halfWidth: 48, run: r, scale: 2)
        let inkStart = left + r.inkLeft, inkEnd = inkStart + r.inkWidth
        XCTAssertEqual(inkStart, 48 - inkEnd, accuracy: 0.5)
        XCTAssertEqual((left * 2).rounded(), left * 2)
        XCTAssertEqual(ReadoutLayout.snap(3.3, scale: 1), 3)
        XCTAssertEqual(ReadoutLayout.snap(3.3, scale: 2), 3.5)
    }

    func testBadgeTopIsRaisedAboveTheCapHeight() {
        let y = ReadoutLayout.badgeOriginY(baseline: 6.5, capHeight: 8.5, scale: 2)
        XCTAssertEqual(y + ReadoutMetrics.badgeSize, 6.5 + 8.5 + 3, accuracy: 0.25)
    }

    func testOneInkAtEveryLevelAndContrastStrengthens() {
        XCTAssertEqual(ReadoutMetrics.letterAlpha(pill: false, dark: true, increasedContrast: false), 0.6)
        XCTAssertEqual(ReadoutMetrics.letterAlpha(pill: false, dark: false, increasedContrast: false), 0.7)
        XCTAssertEqual(ReadoutMetrics.letterAlpha(pill: true, dark: true, increasedContrast: false), 0.75)
        XCTAssertEqual(ReadoutMetrics.letterAlpha(pill: true, dark: false, increasedContrast: false), 0.77)
        XCTAssertEqual(ReadoutMetrics.letterAlpha(pill: true, dark: true, increasedContrast: true), 1)
        XCTAssertEqual(ReadoutMetrics.pillFillAlpha(dark: true, increasedContrast: false), 0.135)
        XCTAssertEqual(ReadoutMetrics.pillFillAlpha(dark: false, increasedContrast: false), 0.093)
        XCTAssertEqual(ReadoutMetrics.pillFillAlpha(dark: true, increasedContrast: true), 0.25)
        XCTAssertEqual(ReadoutMetrics.pillFillAlpha(dark: false, increasedContrast: true), 0.19)
    }
}
