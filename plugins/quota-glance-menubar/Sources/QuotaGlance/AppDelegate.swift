import AppKit
import GlanceCore
import Sparkle

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuItemValidation {
    private let updaterController = SPUStandardUpdaterController(startingUpdater: false, updaterDelegate: nil, userDriverDelegate: nil)
    private let settings = AppSettings()
    private let dashboard = DashboardViewController()
    private let popover = NSPopover()
    private var statusItem: NSStatusItem!
    private var keyMonitor: Any?
    private var appearanceObservation: NSKeyValueObservation?
    private var contrastObserver: NSObjectProtocol?
    private let logos = ProviderLogos.bundled
    private lazy var settingsWindow = SettingsWindowController(settings: settings, updater: updaterController.updater, logos: logos) { [weak self] location in
        guard let self else { return }
        self.dashboard.configure(location)
        self.dashboard.readout.setEnabled(self.settings.readout.style != .iconOnly)
        self.updateReadout()
        self.installApplicationMenu()
        self.showPopover()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        updaterController.startUpdater()
        installApplicationMenu()
        dashboard.onSettings = { [weak self] in self?.showSettings() }
        popover.contentViewController = dashboard
        popover.contentSize = NSSize(width: 400, height: 620)
        popover.behavior = .transient
        popover.appearance = NSAppearance(named: .darkAqua)

        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        if let button = statusItem.button {
            button.image = NSImage(systemSymbolName: "chart.bar.xaxis", accessibilityDescription: "Quota Glance")
            button.image?.isTemplate = true
            button.toolTip = "Quota Glance — right-click for settings"
            button.target = self
            button.action = #selector(statusItemClicked)
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
            // Template alphas are baked into the drawn readout: redraw when the
            // bar turns light or dark (on Tahoe, also when the wallpaper does).
            appearanceObservation = button.observe(\.effectiveAppearance) { [weak self] _, _ in
                Task { @MainActor [weak self] in self?.updateReadout() }
            }
        }
        contrastObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.accessibilityDisplayOptionsDidChangeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in self?.updateReadout() }
        }
        dashboard.readout.onChange = { [weak self] in
            guard let self else { return }
            self.settings.adoptFirstSummary(self.dashboard.readout.state.windows)
            self.updateReadout()
        }
        if let location = settings.location { dashboard.configure(location) }
        dashboard.readout.setEnabled(settings.readout.style != .iconOnly)
        updateReadout()
        keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] event in
            if event.keyCode == 53, self?.popover.isShown == true {
                self?.popover.performClose(nil)
                return nil
            }
            return event
        }
        if settings.location == nil { showSettings() }
    }

    func applicationWillTerminate(_ notification: Notification) {
        dashboard.readout.stop()
        if let keyMonitor { NSEvent.removeMonitor(keyMonitor) }
        appearanceObservation?.invalidate()
        if let contrastObserver { NSWorkspace.shared.notificationCenter.removeObserver(contrastObserver) }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if settings.location == nil { showSettings() } else { showPopover() }
        return true
    }

    private func updateReadout() {
        let readout = settings.readout
        let state = dashboard.readout.state
        if let button = statusItem.button {
            switch readout.style {
            case .iconOnly, .percent:
                // Icon only and Percent keep 0.3's drawing: the symbol, plus " 59%" as the title.
                let selection = readout.style == .percent ? readout.windows.first : nil
                let value = state.presentation(for: selection)
                statusItem.length = selection == nil ? NSStatusItem.squareLength : NSStatusItem.variableLength
                button.image = ReadoutRenderer.appIcon()
                button.title = value.text.isEmpty ? "" : " " + value.text
                button.imagePosition = selection == nil ? .imageOnly : .imageLeading
                button.font = .monospacedDigitSystemFont(ofSize: 12, weight: .medium)
                button.toolTip = value.detail
                button.setAccessibilityLabel("Quota Glance. " + value.detail)
            case .letteredPair, .splitPill:
                let cells = state.cells(for: readout.windows)
                let dark = button.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
                let options = ReadoutRenderer.Options(
                    style: readout.style, badges: readout.badgeVisible, appIcon: readout.showsAppIcon, dark: dark,
                    increasedContrast: NSWorkspace.shared.accessibilityDisplayShouldIncreaseContrast)
                statusItem.length = NSStatusItem.variableLength
                button.title = ""
                button.image = ReadoutRenderer.image(cells: cells, options: options, marks: marks(for: cells))
                button.imagePosition = .imageOnly
                button.toolTip = ReadoutText.tooltip(cells)
                button.setAccessibilityLabel(ReadoutText.accessibilityLabel(cells))
            }
        }
        settingsWindow.updateQuotas(state)
    }

    private func marks(for cells: [ReadoutCell]) -> [String: LogoMark] {
        var marks: [String: LogoMark] = [:]
        for provider in Set(cells.map(\.selection.providerID)) {
            marks[provider] = logos.mark(for: provider)
        }
        return marks
    }

    @objc private func statusItemClicked() {
        let event = NSApp.currentEvent
        if event?.type == .rightMouseUp || event?.modifierFlags.contains(.control) == true {
            popover.performClose(nil)
            guard let button = statusItem.button else { return }
            contextMenu().popUp(positioning: nil, at: NSPoint(x: 0, y: button.bounds.minY), in: button)
        } else if popover.isShown {
            popover.performClose(nil)
        } else if settings.location == nil {
            showSettings()
        } else {
            showPopover()
        }
    }

    private func showPopover() {
        guard let button = statusItem.button, !popover.isShown else { return }
        // Keep the chosen size unless the current display cannot accommodate it.
        let available = button.window?.screen?.visibleFrame.size ?? NSSize(width: 400, height: 620)
        popover.contentSize = NSSize(width: min(400, available.width - 24), height: min(620, available.height - 24))
        NSApp.activate(ignoringOtherApps: true)
        popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
        popover.contentViewController?.view.window?.makeKey()
        dashboard.prepareToShow()
    }

    @objc private func showSettings() {
        popover.performClose(nil)
        settingsWindow.present()
    }

    @objc private func openDashboard() {
        dashboard.goToDashboard()
        showPopover()
    }

    @objc private func checkForUpdates() {
        popover.performClose(nil)
        NSApp.activate(ignoringOtherApps: true)
        updaterController.checkForUpdates(nil)
    }

    func validateMenuItem(_ menuItem: NSMenuItem) -> Bool {
        switch menuItem.action {
        case #selector(checkForUpdates): return updaterController.updater.canCheckForUpdates
        case #selector(openDashboard), #selector(reloadPage), #selector(openInBrowser): return settings.location != nil
        default: return true
        }
    }

    @objc private func reloadPage() { dashboard.reloadPage() }

    @objc private func openInBrowser() {
        guard let url = settings.location?.url else { return }
        NSWorkspace.shared.open(url)
    }

    private func contextMenu() -> NSMenu {
        let menu = NSMenu()
        menu.autoenablesItems = true
        for (title, action, key) in [
            ("Show Dashboard", #selector(openDashboard), ""),
            ("Reload Page", #selector(reloadPage), "r"),
            ("Open in Browser", #selector(openInBrowser), ""),
        ] {
            let item = NSMenuItem(title: title, action: action, keyEquivalent: key)
            item.target = self
            item.isEnabled = settings.location != nil
            menu.addItem(item)
        }
        menu.addItem(.separator())
        let preferences = NSMenuItem(title: "Settings…", action: #selector(showSettings), keyEquivalent: ",")
        preferences.target = self
        menu.addItem(preferences)
        let updates = NSMenuItem(title: "Check for Updates…", action: #selector(checkForUpdates), keyEquivalent: "")
        updates.target = self
        menu.addItem(updates)
        menu.addItem(.separator())
        menu.addItem(withTitle: "Quit Quota Glance", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        return menu
    }

    private func installApplicationMenu() {
        // Accessory apps still need an Edit menu for paste in WebKit and settings.
        let menu = NSMenu()
        let appItem = NSMenuItem()
        appItem.submenu = contextMenu()
        menu.addItem(appItem)
        let editItem = NSMenuItem()
        let edit = NSMenu(title: "Edit")
        for (title, selector, key) in [
            ("Undo", "undo:", "z"), ("Redo", "redo:", "Z"),
            ("Cut", "cut:", "x"), ("Copy", "copy:", "c"),
            ("Paste", "paste:", "v"), ("Select All", "selectAll:", "a"),
        ] {
            edit.addItem(withTitle: title, action: NSSelectorFromString(selector), keyEquivalent: key)
        }
        editItem.submenu = edit
        menu.addItem(editItem)
        NSApp.mainMenu = menu
    }
}
