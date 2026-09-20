import AppKit
import GlanceCore

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private let settings = AppSettings()
    private let dashboard = DashboardViewController()
    private let popover = NSPopover()
    private var statusItem: NSStatusItem!
    private var keyMonitor: Any?
    private lazy var settingsWindow = SettingsWindowController(settings: settings) { [weak self] location in
        self?.dashboard.configure(location)
        self?.installApplicationMenu()
        self?.showPopover()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        installApplicationMenu()
        dashboard.onSettings = { [weak self] in self?.showSettings() }
        if let location = settings.location { dashboard.configure(location) }
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
        }
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
        if let keyMonitor { NSEvent.removeMonitor(keyMonitor) }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if settings.location == nil { showSettings() } else { showPopover() }
        return true
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

    @objc private func reloadPage() { dashboard.reloadPage() }

    @objc private func openInBrowser() {
        guard let url = settings.location?.url else { return }
        NSWorkspace.shared.open(url)
    }

    private func contextMenu() -> NSMenu {
        let menu = NSMenu()
        menu.autoenablesItems = false
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
