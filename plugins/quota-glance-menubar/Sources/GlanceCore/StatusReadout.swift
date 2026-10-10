import Foundation

/// How the Lettered pair or Split pill is drawn. The template alphas depend
/// on `dark` and `increasedContrast`, so both are part of the drawing.
public struct ReadoutOptions: Equatable {
    public var style: ReadoutStyle
    public var badges: Bool
    public var appIcon: Bool
    public var dark: Bool
    public var increasedContrast: Bool

    public init(style: ReadoutStyle, badges: Bool, appIcon: Bool, dark: Bool, increasedContrast: Bool) {
        self.style = style
        self.badges = badges
        self.appIcon = appIcon
        self.dark = dark
        self.increasedContrast = increasedContrast
    }
}

/// Everything the status item shows, as one value the app compares with what
/// it already shows. Each write to the item makes AppKit redraw its copies on
/// the other displays' menu bars, and each copy briefly gives the button that
/// bar's appearance and posts an appearance change. So a value equal to the
/// shown one must cause no writes at all, or the app redraws forever.
public struct StatusReadout: Equatable {
    public enum Length: Equatable { case square, variable }

    public enum Image: Equatable {
        /// The chart symbol: alone in Icon only, before the title in Percent.
        case appIcon
        /// The Lettered pair or Split pill. `logos` names the providers whose badge is drawn.
        case drawn(cells: [ReadoutCell], options: ReadoutOptions, logos: Set<String>)
    }

    public var length: Length
    public var image: Image
    /// " 59%" after the symbol in Percent; empty otherwise.
    public var title: String
    /// Percent puts the symbol before the title; the other styles show the image only.
    public var imageLeading: Bool
    public var toolTip: String
    public var accessibilityLabel: String

    /// `dark` and `increasedContrast` matter only to the drawn styles, so an
    /// appearance change leaves Icon only and Percent as they are. `hasLogo`
    /// says whether a provider has a bundled logo to badge with.
    public init(readout: MenuBarReadout, state: QuotaReadoutState, dark: Bool, increasedContrast: Bool,
                hasLogo: (String) -> Bool, at now: Date = Date()) {
        switch readout.style {
        case .iconOnly, .percent:
            // Icon only and Percent keep 0.3's drawing: the symbol, plus " 59%" as the title.
            let selection = readout.style == .percent ? readout.windows.first : nil
            let value = state.presentation(for: selection, at: now)
            length = selection == nil ? .square : .variable
            image = .appIcon
            title = value.text.isEmpty ? "" : " " + value.text
            imageLeading = selection != nil
            toolTip = value.detail
            accessibilityLabel = "Quota Glance. " + value.detail
        case .letteredPair, .splitPill:
            let cells = state.cells(for: readout.windows, at: now)
            let options = ReadoutOptions(style: readout.style, badges: readout.badgeVisible, appIcon: readout.showsAppIcon,
                                         dark: dark, increasedContrast: increasedContrast)
            let logos = options.badges ? Set(cells.map(\.selection.providerID).filter(hasLogo)) : []
            length = .variable
            image = .drawn(cells: cells, options: options, logos: logos)
            title = ""
            imageLeading = false
            toolTip = ReadoutText.tooltip(cells)
            accessibilityLabel = ReadoutText.accessibilityLabel(cells)
        }
    }

    /// The status item properties a write must set.
    public struct Changes: OptionSet {
        public let rawValue: Int
        public init(rawValue: Int) { self.rawValue = rawValue }

        public static let length = Changes(rawValue: 1 << 0)
        public static let image = Changes(rawValue: 1 << 1)
        public static let title = Changes(rawValue: 1 << 2)
        public static let imagePosition = Changes(rawValue: 1 << 3)
        public static let toolTip = Changes(rawValue: 1 << 4)
        public static let accessibilityLabel = Changes(rawValue: 1 << 5)
        public static let all: Changes = [.length, .image, .title, .imagePosition, .toolTip, .accessibilityLabel]
    }

    /// What differs from `shown`, or everything when nothing is shown yet.
    /// Empty when the two are equal.
    public func changes(from shown: StatusReadout?) -> Changes {
        guard let shown else { return .all }
        var changes: Changes = []
        if length != shown.length { changes.insert(.length) }
        if image != shown.image { changes.insert(.image) }
        if title != shown.title { changes.insert(.title) }
        if imageLeading != shown.imageLeading { changes.insert(.imagePosition) }
        if toolTip != shown.toolTip { changes.insert(.toolTip) }
        if accessibilityLabel != shown.accessibilityLabel { changes.insert(.accessibilityLabel) }
        return changes
    }
}
