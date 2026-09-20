import Foundation

public struct QuotaSelection: Codable, Equatable, Hashable {
    public let providerID: String
    public let rowID: String
    public init(providerID: String, rowID: String) {
        self.providerID = providerID
        self.rowID = rowID
    }
}

public struct QuotaWindow: Decodable, Equatable {
    public let selection: QuotaSelection
    public let title: String
    public let remainingPercent: Int?
}

public struct QuotaReadoutSnapshot: Decodable {
    public let stale: Bool
    public let windows: [QuotaWindow]
}

/// Only a current, successful reading may appear as an unqualified percentage.
/// The last window list is retained across failures so the saved choice survives.
public struct QuotaReadoutState {
    public private(set) var windows: [QuotaWindow] = []
    private var receivedAt: Date?
    private var stale = false
    private var unavailable = true

    public init() {}

    public mutating func receive(_ snapshot: QuotaReadoutSnapshot, at date: Date = Date()) {
        guard snapshot.windows.count <= 500,
              Set(snapshot.windows.map(\.selection)).count == snapshot.windows.count,
              snapshot.windows.allSatisfy({
                  !$0.title.isEmpty && $0.title.count <= 512
                      && !$0.selection.providerID.isEmpty && !$0.selection.rowID.isEmpty
                      && ($0.remainingPercent.map { (0...100).contains($0) } ?? true)
              }) else {
            markUnavailable()
            return
        }
        windows = snapshot.windows
        receivedAt = date
        stale = snapshot.stale
        unavailable = false
    }

    public mutating func markUnavailable() { unavailable = true }

    public func presentation(for selection: QuotaSelection?, at now: Date = Date()) -> (text: String, detail: String) {
        guard let selection else { return ("", "Quota Glance — right-click for settings") }
        let window = windows.first { $0.selection == selection }
        let title = window?.title ?? "Selected quota"
        guard !unavailable, !stale, let receivedAt,
              (0...150).contains(now.timeIntervalSince(receivedAt)),
              let percent = window?.remainingPercent else {
            return ("—%", "\(title): no current reading. Open the dashboard to check its connection and sign-in.")
        }
        return ("\(percent)%", "\(title): \(percent)% remaining. Updates every minute while your Mac is awake.")
    }
}
