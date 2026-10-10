import Foundation

/// Whether a press on the status item is the press that just closed its
/// transient popover, which then stays closed.
///
/// A press on the item closes an open popover and leaves it closed. The
/// transient behaviour may close it first, as a click outside it, and from
/// then on `isShown` is false. So the press also counts as having closed it
/// when the close began after the press, or under half a second before it
/// with the pointer on the item: on macOS 27 the click can reach the popover
/// about 90 ms before it reaches the button.
///
/// Times are seconds since startup, the clock of event timestamps and of
/// system uptime alike. Pure state. The app records each close as it begins,
/// and clears it after every press and after each close of its own.
public struct PopoverPressGuard: Equatable {
    /// How far apart a close and a press may be and still be one press.
    public static let interval: TimeInterval = 0.5

    private struct Close: Equatable {
        let at: TimeInterval
        let pointerOnItem: Bool
    }

    private var lastClose: Close?

    public init() {}

    /// The popover began closing at `time`, with the pointer on the status
    /// item or elsewhere.
    public mutating func recordClose(at time: TimeInterval, pointerOnItem: Bool) {
        lastClose = Close(at: time, pointerOnItem: pointerOnItem)
    }

    /// Forgets the last close. A press is handled (a close it made is its
    /// own, so a quick second press reopens), or the app closed the popover
    /// for its own reason, such as Escape, so a click straight after still
    /// opens it.
    public mutating func clear() {
        lastClose = nil
    }

    /// True if the press at `pressedAt` (nil without an event) is the one
    /// that closed the popover, asked at `now`.
    public func pressClosedPopover(pressedAt: TimeInterval?, now: TimeInterval) -> Bool {
        guard let lastClose else { return false }
        if let pressedAt, (0..<Self.interval).contains(lastClose.at - pressedAt) { return true }
        return lastClose.pointerOnItem && now - lastClose.at < Self.interval
    }
}
