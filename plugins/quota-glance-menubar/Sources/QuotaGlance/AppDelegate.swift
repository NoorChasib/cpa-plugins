import AppKit
import GlanceCore
import Sparkle

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuItemValidation, NSPopoverDelegate {
    private let updaterController = SPUStandardUpdaterController(startingUpdater: false, updaterDelegate: nil, userDriverDelegate: nil)
    private let settings = AppSettings()
    private let dashboard = DashboardViewController()
    private let popover = NSPopover()
    private var statusItem: NSStatusItem!
    private var readoutPresenter: StatusReadoutPresenter?
    private var contrastObserver: NSObjectProtocol?
    /// When the popover last began closing (system uptime), and whether the
    /// pointer was on the status item. The app's own closes clear it.
    private var pressGuard = PopoverPressGuard()
    private let logos = ProviderLogos.bundled
    /// Built on first use, so launch and readout updates never pay for a hidden window.
    private var settingsController: SettingsWindowController?
    private var settingsWindow: SettingsWindowController {
        if let settingsController { return settingsController }
        let controller = SettingsWindowController(settings: settings, updater: updaterController.updater, logos: logos) { [weak self] location in
            guard let self else { return }
            self.dashboard.configure(location)
            self.dashboard.readout.setEnabled(self.settings.readout.style != .iconOnly)
            self.updateReadout()
            self.installApplicationMenu()
            self.showPopover()
        }
        controller.onNeedsWindows = { [weak self] in self?.dashboard.readout.requestWindows() }
        controller.updateQuotas(dashboard.readout.state)
        settingsController = controller
        return controller
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        updaterController.startUpdater()
        installApplicationMenu()
        dashboard.onSettings = { [weak self] in self?.showSettings() }
        // The page sees Escape first, so its own dialogs close before the popover does.
        dashboard.onEscape = { [weak self] in self?.closePopover() }
        popover.contentViewController = dashboard
        popover.contentSize = NSSize(width: 400, height: 620)
        popover.behavior = .transient
        popover.appearance = NSAppearance(named: .darkAqua)
        popover.delegate = self

        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem = item
        if let button = item.button {
            button.target = self
            button.action = #selector(statusItemClicked)
            // Act on the press, as system menu bar items do. A right-click acts
            // on release: sent on right-mouse-down, the action can leave the
            // button highlighted, and the next click then only clears it.
            button.sendAction(on: [.leftMouseDown, .rightMouseUp])
            readoutPresenter = StatusReadoutPresenter(button: button, logos: logos) { item.length = $0 }
        }
        contrastObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.accessibilityDisplayOptionsDidChangeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in self?.updateReadout() }
        }
        dashboard.readout.onChange = { [weak self] in
            guard let self else { return }
            if self.settings.adoptFirstSummary(self.dashboard.readout.state.windows) {
                // An open Settings window must not save the old default pair back.
                self.settingsController?.savedWindowsAdopted(self.settings.readout.windows)
            }
            self.updateReadout()
        }
        if let location = settings.location { dashboard.configure(location) }
        dashboard.readout.setEnabled(settings.readout.style != .iconOnly)
        updateReadout()
        if settings.location == nil { showSettings() }
    }

    func applicationWillTerminate(_ notification: Notification) {
        dashboard.readout.stop()
        readoutPresenter?.stop()
        if let contrastObserver { NSWorkspace.shared.notificationCenter.removeObserver(contrastObserver) }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if settings.location == nil { showSettings() } else { showPopover() }
        return true
    }

    /// Settings, readings and Increase Contrast. Appearance changes go to the
    /// presenter alone, and it writes only what changed.
    private func updateReadout() {
        readoutPresenter?.show(settings.readout, state: dashboard.readout.state)
        settingsController?.updateQuotas(dashboard.readout.state)
    }

    @objc private func statusItemClicked() {
        let event = NSApp.currentEvent
        // A close made here is this press's own, so a quick second press reopens.
        defer { pressGuard.clear() }
        if event?.type == .rightMouseUp || event?.modifierFlags.contains(.control) == true {
            popover.performClose(nil)
            guard let button = statusItem.button else { return }
            contextMenu().popUp(positioning: nil, at: NSPoint(x: 0, y: button.bounds.minY), in: button)
        } else if popover.isShown {
            popover.performClose(nil)
        } else if pressGuard.pressClosedPopover(pressedAt: event?.timestamp, now: ProcessInfo.processInfo.systemUptime) {
            // The transient behaviour closed it for this press: leave it closed.
            return
        } else if settings.location == nil {
            showSettings()
        } else {
            showPopover()
        }
    }

    /// Every close, whatever began it, so a press arriving just after the
    /// transient behaviour's own close can tell it was that press's
    /// (PopoverPressGuard). Event timestamps and system uptime both count
    /// from startup.
    func popoverWillClose(_ notification: Notification) {
        pressGuard.recordClose(at: ProcessInfo.processInfo.systemUptime, pointerOnItem: pointerIsOnStatusItem)
    }

    private var pointerIsOnStatusItem: Bool {
        guard let button = statusItem.button, let window = button.window else { return false }
        return window.convertToScreen(button.convert(button.bounds, to: nil)).contains(NSEvent.mouseLocation)
    }

    /// A close for the app's own reason, such as Escape, is no press's, so a
    /// click on the icon straight after it still opens the popover.
    private func closePopover() {
        popover.performClose(nil)
        pressGuard.clear()
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
        closePopover()
        settingsWindow.present()
    }

    @objc private func openDashboard() {
        dashboard.showDashboard()
        showPopover()
    }

    @objc private func checkForUpdates() {
        closePopover()
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
