import Foundation

/// The signed-off board's metrics for the Lettered pair and Split pill
/// (`quota-glance/web/design/menubar-readout/refined.html`, "Building it"),
/// and the Stacked pair and Stacked pill's from the mockup approved for
/// 0.6.0 (not on that board). All values are points. The app measures glyphs
/// with Core Text and passes the numbers here, so the arithmetic is testable
/// without AppKit.
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

    /// Stacked pair and Stacked pill: the number, then a stack of the logo
    /// over the letter, centred on the digits. Number 15pt, letter 7.5pt.
    public static let stackedFontSize = 15.0
    public static let stackLetterSize = 7.5
    /// The logo's square, on top, and the gap down to the letter.
    public static let stackLogoSize = 8.0
    public static let stackGap = 1.0
    /// Number to stack.
    public static let stackNumberGap = 2.5
    /// Stacked pair: space between slots, with or without logos.
    public static let stackedPairSlotGap = 8.0
    /// Stacked pill: the Split pill's box, taller for the stack.
    public static let stackedPillHeight = 20.0
    public static let stackedPillRadius = 6.0
    /// Ink to each edge of the widest half, for a two-digit reading.
    public static let stackedPillPadding = 5.0

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

/// What a pill half centres: its content's ink, from the content's origin.
public protocol PillContent {
    var inkLeft: Double { get }
    var inkWidth: Double { get }
}

/// One half's content: letter, gap, number, and the badge if drawn. Advances
/// come from the font; side bearings from the glyphs' ink (Core Text image
/// bounds), and the badge's right inset from the logo's ink.
public struct ReadoutRun: Equatable, PillContent {
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

/// One Stacked pair slot's or Stacked pill half's content: number, gap, and
/// the stack, a column as wide as the logo's square or the letter's ink,
/// whichever is wider, with each centred in it. The number's advance and
/// left bearing come from the font, the letter's ink from its glyphs, and
/// the logo's right inset from the logo's ink.
public struct StackedRun: Equatable, PillContent {
    public var numberAdvance: Double
    public var numberLeftBearing: Double
    public var letterInkWidth: Double
    /// Empty room at the logo's right edge, in points; nil without a logo.
    public var logoRightInset: Double?

    public init(numberAdvance: Double, numberLeftBearing: Double, letterInkWidth: Double, logoRightInset: Double?) {
        self.numberAdvance = numberAdvance
        self.numberLeftBearing = numberLeftBearing
        self.letterInkWidth = letterInkWidth
        self.logoRightInset = logoRightInset
    }

    /// Without a logo the stack is the letter alone.
    public var stackWidth: Double {
        max(logoRightInset == nil ? 0 : ReadoutMetrics.stackLogoSize, letterInkWidth)
    }

    public var advance: Double { numberAdvance + ReadoutMetrics.stackNumberGap + stackWidth }

    public var inkLeft: Double { numberLeftBearing }
    /// The stack's ink nearest its right edge: the letter's or the logo's.
    public var inkRight: Double {
        let letter = (stackWidth - letterInkWidth) / 2
        guard let logoRightInset else { return letter }
        return min(letter, (stackWidth - ReadoutMetrics.stackLogoSize) / 2 + logoRightInset)
    }
    public var inkWidth: Double { advance - inkLeft - inkRight }
}

/// Where a stack's letter and logo sit, in a non-flipped image.
public struct StackPlacement: Equatable {
    public var letterBaseline: Double
    /// The logo's bottom edge; nil without a logo.
    public var logoOriginY: Double?

    public init(letterBaseline: Double, logoOriginY: Double?) {
        self.letterBaseline = letterBaseline
        self.logoOriginY = logoOriginY
    }
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

    /// Split pill and Stacked pill: every half is as wide as the widest
    /// half's two-digit ink plus the padding each side, rounded to a whole
    /// point. A wider reading (100) fits while it leaves the minimum room at
    /// each edge; otherwise every half grows by the same amount while it shows.
    /// - Parameters:
    ///   - twoDigit: each half's content as if it read two digits.
    ///   - shown: each half's content for the reading drawn.
    ///   - padding: ink to each edge for two digits; the Stacked pill's is 5pt.
    public static func pillHalfWidth<Content: PillContent>(twoDigit: [Content], shown: [Content],
                                                           padding: Double = ReadoutMetrics.pillPadding) -> Double {
        let fixed = ((twoDigit.map(\.inkWidth).max() ?? 0) + 2 * padding).rounded()
        let need = shown.map { ($0.inkWidth + 2 * ReadoutMetrics.pillMinimumRoom).rounded(.up) }.max() ?? 0
        return max(fixed, need)
    }

    /// Where a half's content starts so its ink is centred, on the pixel grid.
    public static func pillContentOrigin<Content: PillContent>(halfWidth: Double, run: Content, scale: Double) -> Double {
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

    /// Stacked pair: a whole-point slot sized for two digits, so 94, 6 and "—"
    /// are the same width and the stack never moves with the reading; a 100
    /// widens its own slot.
    public static func stackedSlotWidth(numberAdvance: Double, twoDigitAdvance: Double, stackWidth: Double) -> Double {
        (max(twoDigitAdvance, numberAdvance) + ReadoutMetrics.stackNumberGap + stackWidth).rounded(.up)
    }

    /// Where a Stacked pair slot's content starts. Spare room goes before the
    /// number, which sits against its stack, and the stack ends at the slot's
    /// end, so it stays put at every reading. Not snapped: the renderer puts
    /// the number and each part of the stack on the pixel grid itself.
    public static func stackedContentOrigin(slotStart: Double, slotWidth: Double, run: StackedRun) -> Double {
        slotStart + slotWidth - run.advance
    }

    /// The stack (the logo, the gap and the letter's cap height, or the
    /// letter alone without a logo) centred on `centre`, the number's
    /// cap-height centre, on the pixel grid.
    public static func stackPlacement(centre: Double, letterCapHeight: Double, logo: Bool, scale: Double) -> StackPlacement {
        let height = letterCapHeight + (logo ? ReadoutMetrics.stackGap + ReadoutMetrics.stackLogoSize : 0)
        let baseline = snap(centre - height / 2, scale: scale)
        return StackPlacement(letterBaseline: baseline,
                              logoOriginY: logo ? snap(baseline + letterCapHeight + ReadoutMetrics.stackGap, scale: scale) : nil)
    }
}
