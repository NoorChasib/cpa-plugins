import AppKit
import GlanceCore
import ServiceManagement

@MainActor
final class SettingsWindowController: NSWindowController {
    private let settings: AppSettings
    private let onSave: (DashboardLocation) -> Void
    private let urlField = NSTextField()
    private let quotaPicker = NSPopUpButton()
    private var windows: [QuotaWindow] = []
    private let feedback = NSTextField(wrappingLabelWithString: "")
    private lazy var loginCheckbox = NSButton(checkboxWithTitle: "Open at login", target: self, action: #selector(toggleLogin))

    init(settings: AppSettings, onSave: @escaping (DashboardLocation) -> Void) {
        self.settings = settings
        self.onSave = onSave
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 490, height: 460),
                              styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "Quota Glance Settings"
        window.isReleasedWhenClosed = false
        super.init(window: window)
        buildContent()
        window.center()
    }

    required init?(coder: NSCoder) { fatalError("Use init(settings:onSave:)") }

    func present() {
        urlField.stringValue = settings.location?.url.absoluteString ?? ""
        rebuildQuotaPicker(selection: settings.quotaSelection)
        updateLoginCheckbox()
        feedback.stringValue = ""
        NSApp.activate(ignoringOtherApps: true)
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        window?.makeFirstResponder(urlField)
    }

    func updateQuotas(_ windows: [QuotaWindow]) {
        let selection = window?.isVisible == true
            ? quotaPicker.selectedItem?.representedObject as? QuotaSelection
            : settings.quotaSelection
        self.windows = windows
        rebuildQuotaPicker(selection: selection)
    }

    private func rebuildQuotaPicker(selection: QuotaSelection?) {
        quotaPicker.removeAllItems()
        quotaPicker.addItem(withTitle: "Icon only")
        for quota in windows {
            let item = NSMenuItem(title: quota.title, action: nil, keyEquivalent: "")
            item.representedObject = quota.selection
            quotaPicker.menu?.addItem(item)
        }
        if let selection {
            if let item = quotaPicker.itemArray.first(where: { ($0.representedObject as? QuotaSelection) == selection }) {
                quotaPicker.select(item)
            } else {
                let item = NSMenuItem(title: "Selected quota (unavailable)", action: nil, keyEquivalent: "")
                item.representedObject = selection
                quotaPicker.menu?.addItem(item)
                quotaPicker.select(item)
            }
        }
    }

    private func buildContent() {
        guard let content = window?.contentView else { return }
        let title = NSTextField(labelWithString: "Your Quota Glance page")
        title.font = .systemFont(ofSize: 18, weight: .semibold)
        let explanation = NSTextField(wrappingLabelWithString:
            "Paste the full dashboard URL. The app displays that page directly, so changes to it appear here too.")
        explanation.textColor = .secondaryLabelColor
        urlField.placeholderString = "https://your-server/v0/resource/plugins/quota-glance/app"
        urlField.setAccessibilityLabel("Dashboard URL")
        urlField.lineBreakMode = .byTruncatingMiddle
        let signIn = NSTextField(wrappingLabelWithString:
            "Sign in once inside the popover using your dashboard password or CPA console. Your session is saved in this app.")
        signIn.font = .systemFont(ofSize: 11)
        signIn.textColor = .secondaryLabelColor
        let quotaLabel = NSTextField(labelWithString: "Menu bar quota")
        quotaPicker.setAccessibilityLabel("Menu bar quota")
        let quotaHelp = NSTextField(wrappingLabelWithString:
            "Show the remaining percentage beside the icon. Updates every minute while your Mac is awake, even with the popover closed. Sign in to the dashboard to load your choices.")
        quotaHelp.font = .systemFont(ofSize: 11)
        quotaHelp.textColor = .secondaryLabelColor
        feedback.font = .systemFont(ofSize: 11)
        feedback.textColor = .systemRed
        let save = NSButton(title: "Save and Open", target: self, action: #selector(saveSettings))
        save.bezelStyle = .rounded
        save.keyEquivalent = "\r"
        let stack = NSStackView(views: [title, explanation, urlField, signIn, quotaLabel, quotaPicker, quotaHelp, loginCheckbox, feedback, save])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 12
        stack.translatesAutoresizingMaskIntoConstraints = false
        content.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -24),
            stack.topAnchor.constraint(equalTo: content.topAnchor, constant: 24),
            stack.bottomAnchor.constraint(lessThanOrEqualTo: content.bottomAnchor, constant: -24),
        ])
        quotaPicker.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        for field in [explanation, urlField, signIn, quotaHelp, feedback] {
            field.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        window?.defaultButtonCell = save.cell as? NSButtonCell
    }

    @objc private func saveSettings() {
        do {
            let location = try DashboardLocation(urlField.stringValue)
            let sameLocation = settings.location == location
            settings.save(location)
            if sameLocation { settings.quotaSelection = quotaPicker.selectedItem?.representedObject as? QuotaSelection }
            close()
            onSave(location)
        } catch {
            feedback.stringValue = error.localizedDescription
            window?.makeFirstResponder(urlField)
        }
    }

    @objc private func toggleLogin() {
        if loginCheckbox.state == .on,
           (try? Bundle.main.bundleURL.resourceValues(forKeys: [.volumeIsReadOnlyKey]))?.volumeIsReadOnly == true {
            feedback.stringValue = "Drag Quota Glance to Applications and open it there before enabling Open at login."
            updateLoginCheckbox()
            return
        }
        do {
            if loginCheckbox.state == .on {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            feedback.stringValue = SMAppService.mainApp.status == .requiresApproval
                ? "Allow Quota Glance in System Settings → General → Login Items."
                : ""
        } catch {
            feedback.stringValue = "Couldn’t change Open at login. Check System Settings → General → Login Items."
        }
        updateLoginCheckbox()
    }

    private func updateLoginCheckbox() {
        let status = SMAppService.mainApp.status
        loginCheckbox.state = (status == .enabled || status == .requiresApproval) ? .on : .off
    }
}
