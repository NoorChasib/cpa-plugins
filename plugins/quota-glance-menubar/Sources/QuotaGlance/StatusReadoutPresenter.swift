import AppKit
import GlanceCore
import os

/// Shows the readout on the status item, writing only what changed. Each
/// write makes AppKit redraw the item's copy on every other display's menu
/// bar: it gives the button that bar's appearance, takes a snapshot and
/// restores it, posting an appearance change each time. Redrawing on those
/// unconditionally kept 0.4.0 at a full core, so an unchanged readout writes
/// nothing (see StatusReadout).
@MainActor
final class StatusReadoutPresenter {
    private let button: NSButton
    private let logos: ProviderLogos
    private let setLength: (CGFloat) -> Void
    private var observation: NSKeyValueObservation?
    /// True while an appearance check is queued, so a burst of changes queues
    /// one. Locked, because KVO doesn't promise the main thread.
    private let checkQueued = OSAllocatedUnfairLock(initialState: false)
    private var readout: MenuBarReadout?
    private var state = QuotaReadoutState()
    private var shown: StatusReadout?

    /// `setLength` sets the status item's length; the button has no access to it.
    init(button: NSButton, logos: ProviderLogos, setLength: @escaping (CGFloat) -> Void) {
        self.button = button
        self.logos = logos
        self.setLength = setLength
        // Set once: only Percent has a title.
        button.font = .monospacedDigitSystemFont(ofSize: 12, weight: .medium)
        // Template alphas are baked into the drawn readout: redraw when the
        // bar turns light or dark (on Tahoe, also when the wallpaper does).
        // Don't read the appearance here, where it can be another bar's
        // during a snapshot: check it on the main queue, after AppKit has
        // restored it.
        let checkQueued = self.checkQueued
        observation = button.observe(\.effectiveAppearance) { [weak self] _, _ in
            let first = checkQueued.withLock { (queued: inout Bool) -> Bool in
                if queued { return false }
                queued = true
                return true
            }
            guard first else { return }
            Task { @MainActor [weak self] in
                checkQueued.withLock { $0 = false }
                self?.render()
            }
        }
    }

    /// New settings, readings or contrast.
    func show(_ readout: MenuBarReadout, state: QuotaReadoutState) {
        self.readout = readout
        self.state = state
        render()
    }

    func stop() {
        observation?.invalidate()
        observation = nil
    }

    private func render() {
        guard let readout else { return }
        let dark = button.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
        let next = StatusReadout(readout: readout, state: state, dark: dark, increasedContrast: increasedContrast(),
                                 hasLogo: { logos.mark(for: $0) != nil })
        let changes = next.changes(from: skipsUnchanged ? shown : nil)
        // No write, so no snapshot and no appearance change to answer.
        guard !changes.isEmpty else { return }
        shown = next
        if changes.contains(.length) {
            setLength(next.length == .square ? NSStatusItem.squareLength : NSStatusItem.variableLength)
        }
        if changes.contains(.image) { button.image = image(for: next.image) }
        if changes.contains(.title) { button.title = next.title }
        if changes.contains(.imagePosition) { button.imagePosition = next.imageLeading ? .imageLeading : .imageOnly }
        if changes.contains(.toolTip) { button.toolTip = next.toolTip }
        if changes.contains(.accessibilityLabel) { button.setAccessibilityLabel(next.accessibilityLabel) }
        writes += 1
        afterWrite?()
    }

    private func image(for image: StatusReadout.Image) -> NSImage? {
        switch image {
        case .appIcon:
            return ReadoutRenderer.appIcon
        case let .drawn(cells, options, providers):
            var marks: [String: LogoMark] = [:]
            for provider in providers { marks[provider] = logos.mark(for: provider) }
            return ReadoutRenderer.image(cells: cells, options: options, marks: marks)
        }
    }

    // Test hooks.
    /// Read on every render; tests replace it.
    var increasedContrast: () -> Bool = { NSWorkspace.shared.accessibilityDisplayShouldIncreaseContrast }
    /// Renders that wrote to the item.
    private(set) var writes = 0
    /// Runs after each write, where tests stand in for AppKit's snapshot.
    var afterWrite: (() -> Void)?
    /// Off writes every render, as 0.4.0 did: the tests' negative control.
    var skipsUnchanged = true
    var shownForTesting: StatusReadout? { shown }
}
