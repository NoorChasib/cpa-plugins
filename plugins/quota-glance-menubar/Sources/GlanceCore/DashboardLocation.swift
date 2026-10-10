import Foundation

/// The configured destination and its same-origin navigation policy.
public struct DashboardLocation: Equatable {
    public let url: URL

    public enum ValidationError: Error, LocalizedError, Equatable {
        case invalidURL
        case embeddedCredential
        case managementRoute

        public var errorDescription: String? {
            switch self {
            case .invalidURL:
                return "Enter a complete http:// or https:// dashboard URL."
            case .embeddedCredential:
                return "Use a URL without a password or token. Sign in on the dashboard itself."
            case .managementRoute:
                return "That is a CPA Management API address, which this app can’t sign in to. Use the dashboard page’s address, ending in /v0/resource/plugins/quota-glance/app."
            }
        }
    }

    public init(_ value: String) throws {
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty,
              !trimmed.contains(where: { $0.isWhitespace }),
              let components = URLComponents(string: trimmed),
              let scheme = components.scheme?.lowercased(),
              ["http", "https"].contains(scheme),
              let host = components.host, !host.isEmpty,
              let url = components.url,
              components.port.map({ (1...65535).contains($0) }) ?? true
        else { throw ValidationError.invalidURL }

        guard components.user == nil, components.password == nil,
              !(components.queryItems ?? []).contains(where: {
                  ["token", "password", "access_token", "managementkey"].contains($0.name.lowercased())
              })
        else { throw ValidationError.embeddedCredential }

        // CPA takes the management key only in a header, which a page load
        // cannot send, and counts every key-less request toward its ban. The
        // dashboard page is served on the resource tree, never under either
        // management tree (/v0/management, or /v8/management in CPA v8).
        let segments = url.standardized.path.lowercased().split(separator: "/")
        guard !zip(segments, segments.dropFirst()).contains(where: { version, tree in
            tree == "management" && version.count > 1 && version.first == "v"
                && version.dropFirst().allSatisfy { ("0"..."9").contains($0) }
        })
        else { throw ValidationError.managementRoute }

        self.url = url
    }

    /// Keep the whole origin available for the CPA console's sign-in flow.
    public func contains(_ candidate: URL) -> Bool {
        candidate.scheme?.lowercased() == url.scheme?.lowercased()
            && candidate.host?.lowercased() == url.host?.lowercased()
            && Self.effectivePort(candidate) == Self.effectivePort(url)
            && candidate.user == nil && candidate.password == nil
    }

    public func isDashboard(_ candidate: URL) -> Bool {
        contains(candidate) && candidate.path == url.path && candidate.query == url.query
    }

    public enum Navigation: Equatable {
        case embedded, browser, blocked
    }

    public func navigation(to candidate: URL, userActivatedLink: Bool) -> Navigation {
        if contains(candidate) { return .embedded }
        if userActivatedLink,
           ["http", "https"].contains(candidate.scheme?.lowercased() ?? ""),
           candidate.host != nil, candidate.user == nil, candidate.password == nil {
            return .browser
        }
        return .blocked
    }

    private static func effectivePort(_ url: URL) -> Int? {
        url.port ?? (url.scheme?.lowercased() == "https" ? 443 : 80)
    }
}
