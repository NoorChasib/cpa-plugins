import Foundation
import GlanceCore

final class AppSettings {
    private let defaults: UserDefaults
    private let locationKey = "dashboardURL"

    /// Resolves the menu bar readout once, migrating 0.3's `menuBarQuota`, and
    /// writes the result back at once so a later URL save cannot change it.
    /// `menuBarQuota` is left in place so reinstalling 0.3.1 behaves as before.
    init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        let resolved = MenuBarReadout.resolve(
            stored: defaults.data(forKey: MenuBarReadout.storageKey),
            legacySelection: defaults.data(forKey: MenuBarReadout.legacySelectionKey),
            hasDashboardURL: defaults.string(forKey: locationKey) != nil)
        if resolved.migrated { store(resolved.readout) }
    }

    var location: DashboardLocation? {
        guard let value = defaults.string(forKey: locationKey) else { return nil }
        return try? DashboardLocation(value)
    }

    var readout: MenuBarReadout {
        get {
            defaults.data(forKey: MenuBarReadout.storageKey)
                .flatMap { try? JSONDecoder().decode(MenuBarReadout.self, from: $0) } ?? .newInstall
        }
        set { store(newValue) }
    }

    private func store(_ value: MenuBarReadout) {
        if let data = try? JSONEncoder().encode(value.normalized()) {
            defaults.set(data, forKey: MenuBarReadout.storageKey)
        }
    }

    /// Saves the dashboard URL and the readout together. A different dashboard
    /// resets the windows to the default pair and keeps the style, badge and icon.
    func save(_ location: DashboardLocation, readout: MenuBarReadout) {
        self.readout = self.location == location ? readout : readout.resettingWindows()
        defaults.set(location.url.absoluteString, forKey: locationKey)
    }

    /// Saves only the URL, keeping the current readout (reset if the dashboard changed).
    func save(_ location: DashboardLocation) {
        save(location, readout: readout)
    }

    /// The first summary of a dashboard without Claude rows replaces the untouched
    /// default pair with its first provider's first two rows. Only the first
    /// summary per dashboard URL counts, so a provider briefly missing later
    /// never rewrites the choice. Returns true if the windows changed.
    @discardableResult
    func adoptFirstSummary(_ windows: [QuotaWindow]) -> Bool {
        guard !windows.isEmpty, let url = location?.url.absoluteString,
              defaults.string(forKey: firstSummaryKey) != url else { return false }
        defaults.set(url, forKey: firstSummaryKey)
        guard let adopted = readout.adoptingFirstSummary(windows) else { return false }
        readout = adopted
        return true
    }

    private let firstSummaryKey = "menuBarReadoutFirstSummary"
}
