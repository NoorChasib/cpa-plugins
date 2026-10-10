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

    // MARK: - Stacked pair and Stacked pill

    private func stacked(number: Double = 17.6, lsb: Double = 0.9, letter: Double = 4.6, logo: Double? = 0.3) -> StackedRun {
        StackedRun(numberAdvance: number, numberLeftBearing: lsb, letterInkWidth: letter, logoRightInset: logo)
    }

    func testStackIsAsWideAsTheLogoOrTheLetterWhicheverIsWider() {
        XCTAssertEqual(stacked().stackWidth, 8)
        XCTAssertEqual(stacked(letter: 10.4).stackWidth, 10.4, "A wide letter (Wsp) widens the column")
        XCTAssertEqual(stacked(logo: nil).stackWidth, 4.6, "Without a logo the stack is the letter alone")
        XCTAssertEqual(stacked().advance, 17.6 + 2.5 + 8, accuracy: 1e-9)
        // The ink ends at the logo's ink (0.3 short of the column), or at the
        // letter's when it is the wider; it starts at the number's.
        XCTAssertEqual(stacked().inkWidth, 28.1 - 0.9 - 0.3, accuracy: 1e-9)
        XCTAssertEqual(stacked(letter: 10.4).inkWidth, 17.6 + 2.5 + 10.4 - 0.9, accuracy: 1e-9)
        XCTAssertEqual(stacked(logo: nil).inkWidth, 17.6 + 2.5 + 4.6 - 0.9, accuracy: 1e-9)
    }

    func testStackedSlotHoldsTwoDigitsAndOnlyAHundredWidensIt() {
        let two = ReadoutLayout.stackedSlotWidth(numberAdvance: 17.6, twoDigitAdvance: 17.6, stackWidth: 8)
        let one = ReadoutLayout.stackedSlotWidth(numberAdvance: 8.8, twoDigitAdvance: 17.6, stackWidth: 8)
        let dash = ReadoutLayout.stackedSlotWidth(numberAdvance: 12.0, twoDigitAdvance: 17.6, stackWidth: 8)
        let hundred = ReadoutLayout.stackedSlotWidth(numberAdvance: 26.4, twoDigitAdvance: 17.6, stackWidth: 8)
        XCTAssertEqual(two, 29)          // 17.6 + 2.5 + 8 = 28.1, rounded up to a whole point
        XCTAssertEqual(one, two)
        XCTAssertEqual(dash, two)
        XCTAssertEqual(hundred, 37)      // 26.4 + 2.5 + 8 = 36.9
        XCTAssertEqual(ReadoutLayout.stackedSlotWidth(numberAdvance: 17.6, twoDigitAdvance: 17.6, stackWidth: 4.6), 25)
    }

    func testStackedSpareRoomGoesBeforeTheNumberAndTheStackStaysPut() {
        let readings = [stacked(), stacked(number: 8.8), stacked(number: 12.0)]
        var columns: [Double] = []
        for run in readings {
            let origin = ReadoutLayout.stackedContentOrigin(slotStart: 40, slotWidth: 29, run: run)
            XCTAssertEqual(origin + run.advance, 69, accuracy: 1e-9, "The stack ends at the slot's end")
            columns.append(origin + run.numberAdvance + ReadoutMetrics.stackNumberGap)
        }
        XCTAssertEqual(columns.map { ($0 * 1000).rounded() }, Array(repeating: 61_000, count: 3), "The stack never moves with the reading")
        XCTAssertGreaterThan(ReadoutLayout.stackedContentOrigin(slotStart: 40, slotWidth: 29, run: readings[1]),
                             ReadoutLayout.stackedContentOrigin(slotStart: 40, slotWidth: 29, run: readings[0]),
                             "A single digit's spare room is before it, not between it and its stack")
    }

    func testStackIsCentredOnTheDigitsOnThePixelGrid() {
        // Centre 11 (the bar's), letter cap height 5.4: the stack is 8 + 1 + 5.4 = 14.4 tall.
        let retina = ReadoutLayout.stackPlacement(centre: 11, letterCapHeight: 5.4, logo: true, scale: 2)
        XCTAssertEqual(retina, StackPlacement(letterBaseline: 4, logoOriginY: 10.5))   // 3.8 → 4; 10.4 → 10.5
        let plain = ReadoutLayout.stackPlacement(centre: 11, letterCapHeight: 5.4, logo: true, scale: 1)
        XCTAssertEqual(plain, StackPlacement(letterBaseline: 4, logoOriginY: 10))       // 10.4 → 10
        for (place, scale) in [(retina, 2.0), (plain, 1.0)] {
            let top = (place.logoOriginY ?? 0) + ReadoutMetrics.stackLogoSize
            XCTAssertEqual((place.letterBaseline + top) / 2, 11, accuracy: 0.5 / scale, "Centred within a pixel")
            XCTAssertEqual(place.letterBaseline * scale, (place.letterBaseline * scale).rounded())
            XCTAssertEqual((place.logoOriginY ?? 0) * scale, ((place.logoOriginY ?? 0) * scale).rounded())
        }
    }

    func testTheLetterAloneIsCentredOnTheDigits() {
        // No logo: the letter's cap height alone is centred, 11 - 2.7 = 8.3.
        XCTAssertEqual(ReadoutLayout.stackPlacement(centre: 11, letterCapHeight: 5.4, logo: false, scale: 2),
                       StackPlacement(letterBaseline: 8.5, logoOriginY: nil))
        XCTAssertEqual(ReadoutLayout.stackPlacement(centre: 11, letterCapHeight: 5.4, logo: false, scale: 1),
                       StackPlacement(letterBaseline: 8, logoOriginY: nil))
    }

    func testStackedPillHalvesUseTheSplitPillsRuleWithItsOwnPadding() {
        let s = stacked(), w = stacked(letter: 6.6)
        // S's ink: 28.1 - 0.9 - 0.3 = 26.9; plus 5 each side = 36.9 → 37.
        let normal = ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s, w], padding: ReadoutMetrics.stackedPillPadding)
        XCTAssertEqual(normal, 37)
        XCTAssertEqual(ReadoutLayout.pillWidth(halfWidth: normal, halves: 3), 113)
        // One digit or a dash is narrower: same halves.
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [stacked(number: 8.8), w],
                                                   padding: ReadoutMetrics.stackedPillPadding), normal)
        // 15pt digits are wide: a 100 (ink 35.7, plus a point each side) cannot
        // keep the minimum room in 37, so every half grows to 38 while it shows.
        let s100 = stacked(number: 26.4)
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s, w], shown: [s100, w], padding: ReadoutMetrics.stackedPillPadding), 38)
        // With a wider letter elsewhere (Wsp, 13pt of ink) the halves are 42
        // and the same 100 fits without changing them.
        let wide = stacked(letter: 13)
        let roomy = ReadoutLayout.pillHalfWidth(twoDigit: [s, wide], shown: [s, wide], padding: ReadoutMetrics.stackedPillPadding)
        XCTAssertEqual(roomy, 42)
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s, wide], shown: [s100, wide], padding: ReadoutMetrics.stackedPillPadding), roomy)
        // The Split pill's own padding is unchanged by default.
        XCTAssertEqual(ReadoutLayout.pillHalfWidth(twoDigit: [s], shown: [s]), (26.9 + 11).rounded())
    }

    func testStackedPillContentIsCentredByInkOnThePixelGrid() {
        let r = stacked()
        let left = ReadoutLayout.pillContentOrigin(halfWidth: 37, run: r, scale: 2)
        let inkStart = left + r.inkLeft, inkEnd = inkStart + r.inkWidth
        XCTAssertEqual(inkStart, 37 - inkEnd, accuracy: 0.5)
        XCTAssertEqual((left * 2).rounded(), left * 2)
    }

    /// The stacked styles' type and spacing, as approved in the 0.6.0 mockup.
    func testStackedMetricsMatchTheApprovedMockup() {
        XCTAssertEqual(ReadoutMetrics.stackedFontSize, 15)
        XCTAssertEqual(ReadoutMetrics.stackLetterSize, 7.5)
        XCTAssertEqual(ReadoutMetrics.stackLogoSize, 8)
        XCTAssertEqual(ReadoutMetrics.stackGap, 1)
        XCTAssertEqual(ReadoutMetrics.stackNumberGap, 2.5)
        XCTAssertEqual(ReadoutMetrics.stackedPairSlotGap, 8)
        XCTAssertEqual(ReadoutMetrics.stackedPillPadding, 5)
    }

    func testStackedPillBoxHoldsTheStack() {
        XCTAssertEqual(ReadoutMetrics.stackedPillHeight, 20)
        XCTAssertEqual(ReadoutMetrics.stackedPillRadius, 6)
        // A 15pt-tall stack centred in the 20pt box keeps 2.5pt above and below.
        let stack = ReadoutMetrics.stackLogoSize + ReadoutMetrics.stackGap + 6
        XCTAssertEqual((ReadoutMetrics.stackedPillHeight - stack) / 2, 2.5)
        // The existing styles' box is unchanged.
        XCTAssertEqual(ReadoutMetrics.pillHeight, 18)
        XCTAssertEqual(ReadoutMetrics.pillRadius, 5)
        XCTAssertEqual(ReadoutMetrics.pillPadding, 5.5)
    }
}
