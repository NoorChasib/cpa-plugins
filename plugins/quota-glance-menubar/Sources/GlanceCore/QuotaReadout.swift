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

    /// A reading counts only while the last summary is fresh, not stale, and under 150 seconds old.
    public func isCurrent(at now: Date = Date()) -> Bool {
        guard !unavailable, !stale, let receivedAt else { return false }
        return (0...150).contains(now.timeIntervalSince(receivedAt))
    }

    /// Whether any summary has listed its windows yet. Until one has, a chosen
    /// window is unknown, not gone.
    public var hasSummary: Bool { !windows.isEmpty }

    /// One cell per chosen window, in order: its letter, provider and the
    /// remaining percent, or nil ("—") for a stale, missing or expired reading.
    /// Letters are computed against every row the summary lists.
    public func cells(for selections: [QuotaSelection], at now: Date = Date()) -> [ReadoutCell] {
        let rows = windows.map(\.selection)
        let current = isCurrent(at: now)
        return selections.map { selection in
            let window = windows.first { $0.selection == selection }
            return ReadoutCell(
                selection: selection,
                letter: ReadoutLetters.letter(for: selection, among: rows),
                percent: current ? window?.remainingPercent : nil,
                title: window?.title ?? fallbackTitle(for: selection),
                available: !hasSummary || window != nil)
        }
    }

    /// The title a window has when the summary doesn't list it: the provider's
    /// title from another of its rows, else quota-glance's own provider and row
    /// titles, so it reads as the dashboard would title it.
    private func fallbackTitle(for selection: QuotaSelection) -> String {
        let provider = windows.first { $0.selection.providerID == selection.providerID }
            .flatMap { $0.title.components(separatedBy: " · ").first }
            ?? QuotaTitles.provider(selection.providerID)
        return "\(provider) · \(QuotaTitles.row(selection.rowID))"
    }

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

/// One window as the Lettered pair and Split pill draw it.
public struct ReadoutCell: Equatable {
    public let selection: QuotaSelection
    public let letter: String
    /// nil draws "—": stale, missing, expired or not in the summary. 0 is a real reading.
    public let percent: Int?
    /// The summary's "Provider · Row" title, or a fallback when the window has left it.
    public let title: String
    /// False when the window is no longer in the summary.
    public let available: Bool

    public init(selection: QuotaSelection, letter: String, percent: Int?, title: String, available: Bool) {
        self.selection = selection
        self.letter = letter
        self.percent = percent
        self.title = title
        self.available = available
    }

    public var text: String { percent.map(String.init) ?? "—" }

    /// "Claude · Weekly (Fable)" → "Claude weekly (Fable)", for VoiceOver.
    public var spokenName: String {
        let parts = title.components(separatedBy: " · ")
        guard parts.count == 2, let first = parts[1].first else { return title }
        return parts[0] + " " + first.lowercased() + parts[1].dropFirst()
    }
}

public enum ReadoutText {
    public static let cadence = "Updates every minute while your Mac is awake."

    /// "Quota Glance. Claude session 94 percent remaining, Codex weekly 66 percent remaining."
    public static func accessibilityLabel(_ cells: [ReadoutCell]) -> String {
        let parts = cells.map { cell in
            cell.percent.map { "\(cell.spokenName) \($0) percent remaining" } ?? "\(cell.spokenName), no current reading"
        }
        return "Quota Glance. " + parts.joined(separator: ", ") + "."
    }

    /// One line per window, then the update cadence.
    public static func tooltip(_ cells: [ReadoutCell]) -> String {
        var lines = cells.map { cell -> String in
            if let percent = cell.percent { return "\(cell.title): \(percent)% remaining." }
            return cell.available ? "\(cell.title): no current reading." : "\(cell.title): not in your dashboard."
        }
        if cells.contains(where: { $0.percent == nil }) {
            lines.append("Open the dashboard to check its connection and sign-in.")
        }
        lines.append(cadence)
        return lines.joined(separator: "\n")
    }
}

/// quota-glance's fallback titles (`providerTitleOf` and `rowTitleOf` in
/// `plugins/quota-glance/internal/aggregate/build.go`), for windows the
/// current summary doesn't list.
public enum QuotaTitles {
    public static func provider(_ id: String) -> String {
        switch id {
        case "claude": return "Claude"
        case "codex": return "Codex"
        case "xai": return "xAI"
        default: return id
        }
    }

    /// `weekly_fable` → "Weekly (Fable)", `model_weekly:sonnet` → "Weekly (sonnet)",
    /// `raw:xai:product/BUILD` → "product/BUILD". Unknown keys are shown as they are.
    public static func row(_ rowID: String) -> String {
        if rowID.hasPrefix("raw:") {
            let rest = rowID.dropFirst(4)
            if let colon = rest.firstIndex(of: ":") { return String(rest[rest.index(after: colon)...]) }
            return String(rest)
        }
        let parts = rowID.split(separator: ":", maxSplits: 1, omittingEmptySubsequences: false)
        let key = String(parts[0])
        let model = parts.count > 1 ? String(parts[1]) : ""
        let base: String
        switch key {
        case "session", "model_session": base = "Session"
        case "weekly", "model_weekly": base = "Weekly"
        case "weekly_fable": base = "Weekly (Fable)"
        case "credits": base = "Credits"
        case "monthly": base = "Monthly"
        default: return rowID
        }
        return model.isEmpty ? base : "\(base) (\(model))"
    }
}
