import Foundation

/// The signed-off board's metrics for the Lettered pair and Split pill
/// (`quota-glance/web/design/menubar-readout/refined.html`, "Building it").
/// All values are points. The app measures glyphs with Core Text and passes
/// the numbers here, so the arithmetic is testable without AppKit.
public enum ReadoutMetrics {
    /// Letter to number.
    public static let letterGap = 1.0
    /// Provider badge: square, after the number's advance, its top raised above the cap height.
    public static let badgeSize = 9.0
    public static let badgeGap = 1.0
    public static let badgeRaise = 3.0
    /// Lettered pair: space between slots, with and without badges.
    public static let pairSlotGapWithBadges = 6.0
    public static let pairSlotGap = 7.0
    /// Split pill: round 1's box.
    public static let pillHeight = 18.0
    public static let pillRadius = 5.0
    public static let pillDivider = 1.0
    /// Ink to each edge of the widest half, for a two-digit reading.
    public static let pillPadding = 5.5
    /// The least room a wider reading ("100") may leave at each edge before every half grows.
    public static let pillMinimumRoom = 1.0
    /// The app icon and the readout.
    public static let appIconGap = 7.5
    /// Type sizes.
    public static let pairFontSize = 12.0
    public static let pillFontSize = 12.5

    /// Template alphas. Letters are lighter than numbers by a fixed amount;
    /// nothing depends on the reading.
    public static func letterAlpha(pill: Bool, dark: Bool, increasedContrast: Bool) -> Double {
        if increasedContrast { return 1 }
        return pill ? (dark ? 0.75 : 0.77) : (dark ? 0.6 : 0.7)
    }

    public static func pillFillAlpha(dark: Bool, increasedContrast: Bool) -> Double {
        increasedContrast ? (dark ? 0.25 : 0.19) : (dark ? 0.135 : 0.093)
    }
}

/// One half's content: letter, gap, number, and the badge if drawn. Advances
/// come from the font; side bearings from the glyphs' ink (Core Text image
/// bounds), and the badge's right inset from the logo's ink.
public struct ReadoutRun: Equatable {
    public var letterAdvance: Double
    public var letterLeftBearing: Double
    public var numberAdvance: Double
    public var numberRightBearing: Double
    /// Empty room at the badge's right edge, in points; nil without a badge.
    public var badgeRightInset: Double?

    public init(letterAdvance: Double, letterLeftBearing: Double, numberAdvance: Double, numberRightBearing: Double, badgeRightInset: Double?) {
        self.letterAdvance = letterAdvance
        self.letterLeftBearing = letterLeftBearing
        self.numberAdvance = numberAdvance
        self.numberRightBearing = numberRightBearing
        self.badgeRightInset = badgeRightInset
    }

    public var advance: Double {
        letterAdvance + ReadoutMetrics.letterGap + numberAdvance
            + (badgeRightInset == nil ? 0 : ReadoutMetrics.badgeGap + ReadoutMetrics.badgeSize)
    }

    public var inkLeft: Double { letterLeftBearing }
    public var inkRight: Double { badgeRightInset ?? numberRightBearing }
    public var inkWidth: Double { advance - inkLeft - inkRight }
}

public enum ReadoutLayout {
    /// Rounds to the device pixel grid (scale 1 or 2).
    public static func snap(_ value: Double, scale: Double) -> Double {
        let s = max(1, scale)
        return (value * s).rounded() / s
    }

    /// Lettered pair: a whole-point slot sized for two digits, so 94, 6 and "—"
    /// are the same width; a 100 widens its own slot. Spare room goes before the letter.
    public static func pairSlotWidth(letterAdvance: Double, numberAdvance: Double, twoDigitAdvance: Double, badge: Bool) -> Double {
        (letterAdvance + ReadoutMetrics.letterGap + max(twoDigitAdvance, numberAdvance)
            + (badge ? ReadoutMetrics.badgeGap + ReadoutMetrics.badgeSize : 0)).rounded(.up)
    }

    public static func pairGap(badges: Bool) -> Double {
        badges ? ReadoutMetrics.pairSlotGapWithBadges : ReadoutMetrics.pairSlotGap
    }

    /// Split pill: every half is as wide as the widest half's two-digit ink
    /// plus the padding each side, rounded to a whole point. A wider reading
    /// (100) fits while it leaves the minimum room at each edge; otherwise
    /// every half grows by the same amount while it shows.
    /// - Parameters:
    ///   - twoDigit: each half's content as if it read two digits.
    ///   - shown: each half's content for the reading drawn.
    public static func pillHalfWidth(twoDigit: [ReadoutRun], shown: [ReadoutRun]) -> Double {
        let fixed = ((twoDigit.map(\.inkWidth).max() ?? 0) + 2 * ReadoutMetrics.pillPadding).rounded()
        let need = shown.map { ($0.inkWidth + 2 * ReadoutMetrics.pillMinimumRoom).rounded(.up) }.max() ?? 0
        return max(fixed, need)
    }

    /// Where a half's content starts so its ink is centred, on the pixel grid.
    public static func pillContentOrigin(halfWidth: Double, run: ReadoutRun, scale: Double) -> Double {
        snap((halfWidth - run.inkWidth) / 2 - run.inkLeft, scale: scale)
    }

    public static func pillWidth(halfWidth: Double, halves: Int) -> Double {
        Double(halves) * halfWidth + Double(max(0, halves - 1)) * ReadoutMetrics.pillDivider
    }

    /// The badge's bottom edge above the image's bottom, in a non-flipped
    /// image: its top sits `badgeRaise` above the cap height, on the pixel grid.
    public static func badgeOriginY(baseline: Double, capHeight: Double, scale: Double) -> Double {
        snap(baseline + capHeight + ReadoutMetrics.badgeRaise - ReadoutMetrics.badgeSize, scale: scale)
    }
}
