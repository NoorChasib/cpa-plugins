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

    var quotaSelection: QuotaSelection? {
        get {
            guard let data = defaults.data(forKey: "menuBarQuota") else { return nil }
            return try? JSONDecoder().decode(QuotaSelection.self, from: data)
        }
        set {
            if let value = newValue, let data = try? JSONEncoder().encode(value) {
                defaults.set(data, forKey: "menuBarQuota")
            } else {
                defaults.removeObject(forKey: "menuBarQuota")
            }
        }
    }

    func save(_ location: DashboardLocation) {
        if self.location != location { quotaSelection = nil }
        defaults.set(location.url.absoluteString, forKey: locationKey)
    }
}
