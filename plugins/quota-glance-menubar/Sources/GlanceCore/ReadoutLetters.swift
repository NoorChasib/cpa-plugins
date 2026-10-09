import Foundation

/// The letter drawn before each number. It belongs to the window, not to the
/// slot: it is derived from the summary's rowId and compared only with the
/// other rows of the same provider, so reordering or choosing other windows
/// never changes it. A new row can: a second model with the same initial
/// turns `Ws` into `Wso`/`Wsp`. Settings re-checks clashes on every summary.
///
/// rowId shapes (quota-cache window keys, quota-glance `rowIDOf`):
/// - a canonical key: session S, weekly W, weekly_fable F, credits C, monthly M;
/// - `model_session:<model>` / `model_weekly:<model>`: S or W plus the model's
///   first lowercase letter (a lowercase `l` is skipped; it reads as 1);
/// - `raw:<provider>:<id>`: the id's initial, or two letters when that initial
///   is S, W, F, C or M or looks like a digit (O, Q, I).
/// When two rows of one provider share the short form, both take the long one.
///
/// `scripts/letters-fixture.mjs` holds the design board's copy of these rules;
/// `Tests/GlanceCoreTests/Fixtures/letters.json` is its output, and the Swift
/// tests check this port against it.
public enum ReadoutLetters {
    private static let kinds: [String: String] = ["session": "S", "weekly": "W", "weekly_fable": "F", "credits": "C", "monthly": "M"]
    private static let modelKinds: [String: String] = ["model_session": "S", "model_weekly": "W"]
    private static let taken: Set<String> = ["S", "W", "F", "C", "M"]
    private static let digitLike: Set<String> = ["O", "Q", "I"]

    /// The short form, and the long form used when another row of the same provider shares the short one.
    public static func forms(rowID: String) -> (short: String, long: String) {
        let (kind, name) = parse(rowID)
        if let letter = kinds[kind] { return (letter, letter) }
        if let letter = modelKinds[kind] { return (letter + small(name, 1), letter + small(name, 2)) }
        // Unknown shapes the board does not draw: fall back to the kind's letters
        // rather than drawing nothing.
        let source = letters(in: name).isEmpty && kind != "raw" ? kind : name
        guard !letters(in: source).isEmpty else { return ("?", "?") }
        let one = initials(source, 1)
        return taken.contains(one) || digitLike.contains(one)
            ? (initials(source, 2), initials(source, 3))
            : (one, initials(source, 2))
    }

    /// The letter for `selection` among the summary's rows (`rows` may omit it,
    /// for a chosen window that has left the summary).
    public static func letter(for selection: QuotaSelection, among rows: [QuotaSelection]) -> String {
        let own = forms(rowID: selection.rowID)
        let shared = rows.contains { other in
            other != selection && other.providerID == selection.providerID && forms(rowID: other.rowID).short == own.short
        }
        return shared ? own.long : own.short
    }

    /// The first two chosen windows that would look alike in the menu bar:
    /// same letters and either the same provider (the badge cannot help) or no
    /// badge drawn. Indexes into `windows`.
    public static func clash(_ windows: [QuotaSelection], letters: [String], badgeVisible: Bool) -> (first: Int, second: Int, sameProvider: Bool)? {
        guard windows.count == letters.count else { return nil }
        for i in windows.indices {
            for j in windows.indices where j > i && letters[i] == letters[j] {
                let same = windows[i].providerID == windows[j].providerID
                if same || !badgeVisible { return (i, j, same) }
            }
        }
        return nil
    }

    private static func parse(_ rowID: String) -> (kind: String, name: String) {
        if rowID.hasPrefix("raw:") {
            let rest = rowID.dropFirst(4)
            if let colon = rest.firstIndex(of: ":") { return ("raw", String(rest[rest.index(after: colon)...])) }
            return ("raw", String(rest))
        }
        if let colon = rowID.firstIndex(of: ":") {
            return (String(rowID[..<colon]), String(rowID[rowID.index(after: colon)...]))
        }
        return (rowID, "")
    }

    private static func letters(in name: String) -> [Character] {
        name.filter { $0.isASCII && $0.isLetter }.map { $0 }
    }

    private static func small(_ name: String, _ count: Int) -> String {
        String(letters(in: name).map { Character($0.lowercased()) }.filter { $0 != "l" }.prefix(count))
    }

    private static func initials(_ name: String, _ count: Int) -> String {
        let all = letters(in: name)
        guard let first = all.first else { return "" }
        return first.uppercased() + String(all.dropFirst().prefix(count - 1)).lowercased()
    }
}
