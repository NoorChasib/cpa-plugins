import AppKit
import GlanceCore
import XCTest
@testable import QuotaGlance

/// 0.4.0's CPU loop without a second display. After each write to a status
/// item, AppKit redraws its copy for every other menu bar: it sets that bar's
/// appearance on the button, takes a snapshot and restores the button's own,
/// posting two effectiveAppearance changes. `afterWrite` stands in for that.
final class StatusReadoutLoopTests: XCTestCase {
    private let logos = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().appendingPathComponent("Resources/Logos")
    private let windows = [
        QuotaSelection(providerID: "claude", rowID: "session"),
        QuotaSelection(providerID: "claude", rowID: "weekly"),
        QuotaSelection(providerID: "codex", rowID: "weekly"),
    ]

    private func state() throws -> QuotaReadoutState {
        let json = """
        {"stale":false,"windows":[
          {"selection":{"providerID":"claude","rowID":"session"},"title":"Claude · Session","remainingPercent":94},
          {"selection":{"providerID":"claude","rowID":"weekly"},"title":"Claude · Weekly","remainingPercent":59},
          {"selection":{"providerID":"codex","rowID":"weekly"},"title":"Codex · Weekly","remainingPercent":36}]}
        """
        var state = QuotaReadoutState()
        state.receive(try JSONDecoder().decode(QuotaReadoutSnapshot.self, from: Data(json.utf8)))
        return state
    }

    private func readout(_ style: ReadoutStyle = .letteredPair) -> MenuBarReadout {
        MenuBarReadout(style: style, windows: windows, badge: .always, showsAppIcon: false)
    }

    /// A plain button on a dark bar, in a window that is never shown.
    @MainActor
    private func makePresenter(skipsUnchanged: Bool = true) -> (StatusReadoutPresenter, NSButton, NSWindow) {
        _ = NSApplication.shared
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 240, height: 22), styleMask: .borderless,
                              backing: .buffered, defer: true)
        window.isReleasedWhenClosed = false
        window.appearance = NSAppearance(named: .darkAqua)
        let button = NSButton(frame: NSRect(x: 0, y: 0, width: 240, height: 22))
        window.contentView?.addSubview(button)
        let presenter = StatusReadoutPresenter(button: button, logos: ProviderLogos(directory: logos)) { _ in }
        presenter.skipsUnchanged = skipsUnchanged
        presenter.increasedContrast = { false }
        // -[NSStatusItem _updateReplicant:] runs later, from a delayed perform,
        // and sets and restores the appearance within one call.
        presenter.afterWrite = { [weak button] in
            DispatchQueue.main.async {
                guard let button else { return }
                let own = button.appearance
                let dark = button.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
                button.appearance = NSAppearance(named: dark ? .aqua : .darkAqua)
                button.appearance = own
            }
        }
        return (presenter, button, window)
    }

    @MainActor
    private func settle() {
        RunLoop.main.run(until: Date().addingTimeInterval(0.5))
    }

    @MainActor
    private func stop(_ presenter: StatusReadoutPresenter) {
        presenter.afterWrite = nil
        presenter.stop()
        settle()
    }

    @MainActor
    func testSnapshotsOfOtherMenuBarsCauseNoFurtherWrites() throws {
        let (presenter, _, window) = makePresenter()
        defer { stop(presenter); window.close() }
        presenter.show(readout(), state: try state())
        settle()
        XCTAssertEqual(presenter.writes, 1, "An unchanged appearance must not write again")
    }

    /// Negative control: proves the stand-in re-sends the notifications. If
    /// this fails, the test above proves nothing.
    @MainActor
    func testWithoutTheComparisonTheStandInLoops() throws {
        let (presenter, _, window) = makePresenter(skipsUnchanged: false)
        defer { stop(presenter); window.close() }
        presenter.show(readout(), state: try state())
        settle()
        XCTAssertGreaterThan(presenter.writes, 50)
    }

    @MainActor
    func testASettledAppearanceChangeRedrawsOnce() throws {
        let (presenter, button, window) = makePresenter()
        defer { stop(presenter); window.close() }
        presenter.show(readout(), state: try state())
        settle()
        XCTAssertEqual(presenter.writes, 1)
        button.appearance = NSAppearance(named: .aqua)
        settle()
        XCTAssertEqual(presenter.writes, 2, "Dark to light redraws once, then settles")
        guard case let .drawn(_, options, _) = presenter.shownForTesting?.image else { return XCTFail("Lettered pair is drawn") }
        XCTAssertFalse(options.dark, "The redraw uses the settled appearance")
    }

    @MainActor
    func testIncreaseContrastRedrawsAndUnchangedDataDoesNot() throws {
        let (presenter, _, window) = makePresenter()
        defer { stop(presenter); window.close() }
        var contrast = false
        presenter.increasedContrast = { contrast }
        let current = try state()
        presenter.show(readout(), state: current)
        settle()
        // The minute timer and repeated snapshots with nothing new.
        presenter.show(readout(), state: current)
        presenter.show(readout(), state: current)
        settle()
        XCTAssertEqual(presenter.writes, 1)
        contrast = true
        presenter.show(readout(), state: current)
        settle()
        XCTAssertEqual(presenter.writes, 2)
        guard case let .drawn(_, options, _) = presenter.shownForTesting?.image else { return XCTFail("Lettered pair is drawn") }
        XCTAssertTrue(options.increasedContrast)
    }

    @MainActor
    func testAppearanceChangesLeavePercentAlone() throws {
        // The symbol is a template image the system tints, so there is nothing to redraw.
        let (presenter, button, window) = makePresenter()
        defer { stop(presenter); window.close() }
        presenter.show(readout(.percent), state: try state())
        settle()
        button.appearance = NSAppearance(named: .aqua)
        settle()
        XCTAssertEqual(presenter.writes, 1)
        XCTAssertEqual(button.title, " 94%")
        XCTAssertEqual(button.imagePosition, .imageLeading)
    }
}
