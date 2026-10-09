import Foundation

/// What the status item shows. `iconOnly` and `percent` are the 0.3 forms;
/// `letteredPair` and `splitPill` draw one to three windows with letters.
public enum ReadoutStyle: String, Codable, CaseIterable {
    case iconOnly, percent, letteredPair, splitPill

    public var title: String {
        switch self {
        case .iconOnly: return "Icon only"
        case .percent: return "Percent"
        case .letteredPair: return "Lettered pair"
        case .splitPill: return "Split pill"
        }
    }

    /// Styles that draw letters, badges and the optional app icon into one image.
    public var isLettered: Bool { self == .letteredPair || self == .splitPill }
}

/// When the provider logo is drawn at the top-right of each number.
public enum BadgeMode: String, Codable, CaseIterable {
    case always, whenProvidersDiffer, off

    public var title: String {
        switch self {
        case .always: return "Always"
        case .whenProvidersDiffer: return "When providers differ"
        case .off: return "Off"
        }
    }

    public func isVisible(forProviders providers: [String]) -> Bool {
        switch self {
        case .always: return true
        case .off: return false
        case .whenProvidersDiffer: return Set(providers).count > 1
        }
    }
}

/// The menu bar readout preference, stored as one JSON value under
/// `menuBarReadout`. Decoding never fails on unknown or missing values; each
/// falls back to its default, and `normalized()` enforces one to three
/// unique, ordered windows.
public struct MenuBarReadout: Codable, Equatable {
    public static let storageKey = "menuBarReadout"
    public static let legacySelectionKey = "menuBarQuota"
    public static let maxWindows = 3
    public static let defaultWindows = [
        QuotaSelection(providerID: "claude", rowID: "session"),
        QuotaSelection(providerID: "claude", rowID: "weekly"),
    ]
    /// A new install: Lettered pair, Claude Session + Claude Weekly, badge Always, icon off.
    public static let newInstall = MenuBarReadout(style: .letteredPair, windows: defaultWindows, badge: .always, showsAppIcon: false)

    public var style: ReadoutStyle
    public var windows: [QuotaSelection]
    public var badge: BadgeMode
    public var showsAppIcon: Bool

    public init(style: ReadoutStyle, windows: [QuotaSelection], badge: BadgeMode = .always, showsAppIcon: Bool) {
        self.style = style
        self.windows = windows
        self.badge = badge
        self.showsAppIcon = showsAppIcon
        self = normalized()
    }

    private enum CodingKeys: String, CodingKey { case style, windows, badge, showsAppIcon }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let style = (try? c.decodeIfPresent(String.self, forKey: .style)).flatMap { $0 }.flatMap(ReadoutStyle.init(rawValue:))
        let badge = (try? c.decodeIfPresent(String.self, forKey: .badge)).flatMap { $0 }.flatMap(BadgeMode.init(rawValue:))
        let icon = (try? c.decodeIfPresent(Bool.self, forKey: .showsAppIcon)).flatMap { $0 }
        // Decode windows one by one so a single malformed entry is dropped, not the list.
        var windows: [QuotaSelection] = []
        if var list = try? c.nestedUnkeyedContainer(forKey: .windows) {
            while !list.isAtEnd {
                if let value = try? list.decode(QuotaSelection.self) {
                    windows.append(value)
                } else if (try? list.decode(Discarded.self)) == nil {
                    break
                }
            }
        }
        self.init(style: style ?? Self.newInstall.style, windows: windows,
                  badge: badge ?? .always, showsAppIcon: icon ?? Self.newInstall.showsAppIcon)
    }

    /// Consumes one array element of any shape, so decoding moves past it.
    private struct Discarded: Decodable {
        init(from decoder: Decoder) throws {}
    }

    /// One to three windows, in order, without duplicates or empty IDs.
    public func normalized() -> MenuBarReadout {
        var seen = Set<QuotaSelection>()
        var unique = windows.filter { !$0.providerID.isEmpty && !$0.rowID.isEmpty && seen.insert($0).inserted }
        if unique.count > Self.maxWindows { unique = Array(unique.prefix(Self.maxWindows)) }
        if unique.isEmpty { unique = Self.defaultWindows }
        var copy = self
        copy.windows = unique
        return copy
    }

    /// Reads the stored preference, migrating 0.3's single `menuBarQuota`.
    /// First match wins:
    /// 1. a stored `menuBarReadout`, as is;
    /// 2. an old `menuBarQuota` → Percent with that window (nothing visible changes);
    /// 3. a saved dashboard URL without `menuBarQuota` (Icon only in 0.3) → Icon only,
    ///    the default pair ready, app icon on;
    /// 4. nothing saved (a new install) → Lettered pair, default pair, Always, icon off.
    /// `migrated` is true when the result did not come from rule 1 and must be
    /// written back at once, so a later URL save cannot change the outcome.
    public static func resolve(stored: Data?, legacySelection: Data?, hasDashboardURL: Bool) -> (readout: MenuBarReadout, migrated: Bool) {
        if let stored, let value = try? JSONDecoder().decode(MenuBarReadout.self, from: stored) {
            return (value, false)
        }
        if let legacySelection, let selection = try? JSONDecoder().decode(QuotaSelection.self, from: legacySelection),
           !selection.providerID.isEmpty, !selection.rowID.isEmpty {
            return (MenuBarReadout(style: .percent, windows: [selection], badge: .always, showsAppIcon: true), true)
        }
        if hasDashboardURL {
            return (MenuBarReadout(style: .iconOnly, windows: defaultWindows, badge: .always, showsAppIcon: true), true)
        }
        return (newInstall, true)
    }

    /// A new dashboard URL resets the windows to the default pair and keeps the style.
    public func resettingWindows() -> MenuBarReadout {
        var copy = self
        copy.windows = Self.defaultWindows
        return copy
    }

    /// If the windows are still the untouched default pair and the summary has
    /// no Claude rows, use its first provider's first two rows instead.
    /// Returns nil when nothing should change.
    public func adoptingFirstSummary(_ available: [QuotaWindow]) -> MenuBarReadout? {
        guard windows == Self.defaultWindows, !available.isEmpty,
              !available.contains(where: { $0.selection.providerID == "claude" }),
              let first = available.first?.selection.providerID else { return nil }
        let rows = available.filter { $0.selection.providerID == first }.prefix(2).map(\.selection)
        var copy = self
        copy.windows = Array(rows)
        return copy.normalized()
    }

    /// Whether badges are drawn for these windows.
    public var badgeVisible: Bool { badge.isVisible(forProviders: windows.map(\.providerID)) }
}
