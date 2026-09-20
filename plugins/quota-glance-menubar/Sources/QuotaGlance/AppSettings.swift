import Foundation
import GlanceCore

final class AppSettings {
    private let defaults: UserDefaults
    private let locationKey = "dashboardURL"

    init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    var location: DashboardLocation? {
        guard let value = defaults.string(forKey: locationKey) else { return nil }
        return try? DashboardLocation(value)
    }

    func save(_ location: DashboardLocation) {
        defaults.set(location.url.absoluteString, forKey: locationKey)
    }
}
