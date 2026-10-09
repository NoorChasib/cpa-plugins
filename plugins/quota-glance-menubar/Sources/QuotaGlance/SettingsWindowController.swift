import AppKit
import GlanceCore
import ServiceManagement
import Sparkle

@MainActor
final class SettingsWindowController: NSWindowController {
    private let settings: AppSettings
    private let updater: SPUUpdater
    private let logos: ProviderLogos
    private let onSave: (DashboardLocation) -> Void
    private let urlField = NSTextField()
    private let feedback = NSTextField(wrappingLabelWithString: "")
    private lazy var loginCheckbox = NSButton(checkboxWithTitle: "Open at login", target: self, action: #selector(toggleLogin))

    // Menu bar section. `draft` follows the controls; Save writes it.
    private var draft = MenuBarReadout.newInstall
    private var state = QuotaReadoutState()
    private let styleControl = NSSegmentedControl(labels: ReadoutStyle.allCases.map(\.title), trackingMode: .selectOne, target: nil, action: nil)
    private let table = NSTableView()
    private let tableScroll = NSScrollView()
    private var tableHeight: NSLayoutConstraint?
    private let addRemove = NSSegmentedControl()
    private let windowsHint = NSTextField(labelWithString: "")
    private let badgePopup = NSPopUpButton()
    private lazy var iconCheckbox = NSButton(checkboxWithTitle: "Show the Quota Glance icon", target: self, action: #selector(iconChanged))
    private let darkPreview = PreviewSwatch(dark: true)
    private let lightPreview = PreviewSwatch(dark: false)
    private let previewWidth = NSTextField(labelWithString: "")
    private let clashWarning = WarningRow()
    private let goneWarning = WarningRow()
    private var grid: NSGridView?
    private static let rowType = NSPasteboard.PasteboardType("com.noorchasib.quota-glance-menubar.window-row")
    private static let rowHeight: CGFloat = 26

    init(settings: AppSettings, updater: SPUUpdater, logos: ProviderLogos, onSave: @escaping (DashboardLocation) -> Void) {
        self.settings = settings
        self.updater = updater
        self.logos = logos
        self.onSave = onSave
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 490, height: 760),
                              styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "Quota Glance Settings"
        window.isReleasedWhenClosed = false
        super.init(window: window)
        buildContent()
        window.center()
    }

    required init?(coder: NSCoder) { fatalError("Use init(settings:updater:logos:onSave:)") }

    func present() {
        urlField.stringValue = settings.location?.url.absoluteString ?? ""
        draft = settings.readout
        refreshMenuBarSection()
        updateLoginCheckbox()
        feedback.stringValue = ""
        NSApp.activate(ignoringOtherApps: true)
        showWindow(nil)
        window?.makeKeyAndOrderFront(nil)
        window?.makeFirstResponder(urlField)
    }

    /// Called on every summary. While Settings is closed the draft follows the
    /// saved choice; while it is open, the user's edits are kept.
    func updateQuotas(_ state: QuotaReadoutState) {
        let rowsChanged = state.windows.map(\.selection) != self.state.windows.map(\.selection)
            || state.windows.map(\.title) != self.state.windows.map(\.title)
        self.state = state
        if window?.isVisible != true {
            draft = settings.readout
            refreshMenuBarSection()
        } else if rowsChanged {
            refreshMenuBarSection()
        } else {
            // Same rows, new readings: leave the table (and any open pop-up) alone.
            refreshPreviewAndWarnings()
        }
    }

    // MARK: - Layout

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
        let signIn = small("Sign in once inside the popover using your dashboard password or CPA console. Your session is saved in this app.")
        feedback.font = .systemFont(ofSize: 11)
        feedback.textColor = .systemRed
        let save = NSButton(title: "Save and Open", target: self, action: #selector(saveSettings))
        save.bezelStyle = .rounded
        save.keyEquivalent = "\r"
        let automaticChecks = NSButton(checkboxWithTitle: "Automatically check for updates", target: nil, action: nil)
        automaticChecks.bind(.value, to: updater, withKeyPath: "automaticallyChecksForUpdates", options: nil)
        let automaticInstall = NSButton(checkboxWithTitle: "Download and install updates automatically", target: nil, action: nil)
        automaticInstall.bind(.value, to: updater, withKeyPath: "automaticallyDownloadsUpdates", options: nil)
        automaticInstall.bind(.enabled, to: updater, withKeyPath: "allowsAutomaticUpdates", options: nil)

        let heading = NSTextField(labelWithString: "Menu bar")
        heading.font = .systemFont(ofSize: 13, weight: .semibold)
        let menuBar = buildMenuBarForm()
        let separators = [NSBox(), NSBox()]
        separators.forEach { $0.boxType = .separator }

        let stack = NSStackView(views: [title, explanation, urlField, signIn, separators[0], heading, menuBar, separators[1],
                                        loginCheckbox, automaticChecks, automaticInstall, feedback, save])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 12
        stack.translatesAutoresizingMaskIntoConstraints = false
        content.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -24),
            stack.topAnchor.constraint(equalTo: content.topAnchor, constant: 20),
            stack.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -24),
        ])
        for view in [explanation, urlField, signIn, separators[0], separators[1], menuBar, feedback] as [NSView] {
            view.widthAnchor.constraint(equalTo: stack.widthAnchor).isActive = true
        }
        window?.defaultButtonCell = save.cell as? NSButtonCell
    }

    private func small(_ text: String) -> NSTextField {
        let field = NSTextField(wrappingLabelWithString: text)
        field.font = .systemFont(ofSize: 11)
        field.textColor = .secondaryLabelColor
        return field
    }

    private func label(_ text: String) -> NSTextField {
        let field = NSTextField(labelWithString: text)
        field.alignment = .right
        return field
    }

    private func buildMenuBarForm() -> NSView {
        styleControl.target = self
        styleControl.action = #selector(styleChanged)
        styleControl.segmentStyle = .rounded
        styleControl.setAccessibilityLabel("Show")

        let column = NSTableColumn(identifier: NSUserInterfaceItemIdentifier("window"))
        column.resizingMask = .autoresizingMask
        table.addTableColumn(column)
        table.headerView = nil
        table.rowHeight = Self.rowHeight
        table.intercellSpacing = NSSize(width: 0, height: 0)
        table.columnAutoresizingStyle = .uniformColumnAutoresizingStyle
        table.usesAlternatingRowBackgroundColors = false
        table.dataSource = self
        table.delegate = self
        table.registerForDraggedTypes([Self.rowType])
        table.draggingDestinationFeedbackStyle = .gap
        table.setAccessibilityLabel("Windows")
        tableScroll.documentView = table
        tableScroll.hasVerticalScroller = false
        tableScroll.borderType = .bezelBorder
        tableScroll.translatesAutoresizingMaskIntoConstraints = false
        let height = tableScroll.heightAnchor.constraint(equalToConstant: Self.rowHeight * 2 + 2)
        height.isActive = true
        tableHeight = height

        addRemove.segmentCount = 2
        addRemove.trackingMode = .momentary
        addRemove.segmentStyle = .smallSquare
        if let add = NSImage(named: NSImage.addTemplateName) { addRemove.setImage(add, forSegment: 0) }
        if let remove = NSImage(named: NSImage.removeTemplateName) { addRemove.setImage(remove, forSegment: 1) }
        addRemove.setWidth(24, forSegment: 0)
        addRemove.setWidth(24, forSegment: 1)
        addRemove.target = self
        addRemove.action = #selector(addOrRemove)
        addRemove.setAccessibilityLabel("Add or remove a window")
        windowsHint.font = .systemFont(ofSize: 11)
        windowsHint.textColor = .secondaryLabelColor
        let dragHint = NSTextField(labelWithString: "Drag rows to reorder.")
        dragHint.font = .systemFont(ofSize: 11)
        dragHint.textColor = .secondaryLabelColor
        let spacer = NSView()
        spacer.setContentHuggingPriority(.defaultLow, for: .horizontal)
        let under = NSStackView(views: [addRemove, windowsHint, spacer, dragHint])
        under.spacing = 8
        let windows = NSStackView(views: [tableScroll, under])
        windows.orientation = .vertical
        windows.alignment = .leading
        windows.spacing = 5
        tableScroll.widthAnchor.constraint(equalTo: windows.widthAnchor).isActive = true
        under.widthAnchor.constraint(equalTo: windows.widthAnchor).isActive = true

        for mode in BadgeMode.allCases {
            badgePopup.addItem(withTitle: mode.title)
            badgePopup.lastItem?.representedObject = mode.rawValue
        }
        badgePopup.target = self
        badgePopup.action = #selector(badgeChanged)
        badgePopup.setAccessibilityLabel("Badge")
        badgePopup.widthAnchor.constraint(equalToConstant: 190).isActive = true
        let badgeRow = NSStackView(views: [badgePopup, smallLabel("Provider logo on each number")])
        badgeRow.spacing = 10

        previewWidth.font = .monospacedDigitSystemFont(ofSize: 11, weight: .regular)
        previewWidth.textColor = .secondaryLabelColor
        let preview = NSStackView(views: [darkPreview, lightPreview, previewWidth])
        preview.spacing = 10
        preview.setAccessibilityLabel("Preview")

        let help = small("Updates every minute while your Mac is awake, even with the popover closed. Sign in to the dashboard to load your choices. The menu bar changes when you click Save and Open.")

        let grid = NSGridView(views: [
            [label("Show"), styleControl],
            [label("Windows"), windows],
            [label("Badge"), badgeRow],
            [NSGridCell.emptyContentView, iconCheckbox],
            [label("Preview"), preview],
            [NSGridCell.emptyContentView, clashWarning],
            [NSGridCell.emptyContentView, goneWarning],
            [NSGridCell.emptyContentView, help],
        ])
        grid.rowSpacing = 10
        grid.columnSpacing = 10
        grid.column(at: 0).xPlacement = .trailing
        grid.column(at: 0).width = 62
        grid.row(at: 1).yPlacement = .top
        grid.row(at: 4).yPlacement = .center
        grid.translatesAutoresizingMaskIntoConstraints = false
        self.grid = grid
        for view in [windows, clashWarning, goneWarning, help] as [NSView] {
            view.translatesAutoresizingMaskIntoConstraints = false
            view.widthAnchor.constraint(equalToConstant: 490 - 48 - 62 - 10).isActive = true
        }
        return grid
    }

    private func smallLabel(_ text: String) -> NSTextField {
        let field = NSTextField(labelWithString: text)
        field.font = .systemFont(ofSize: 11)
        field.textColor = .secondaryLabelColor
        return field
    }

    // MARK: - State → controls

    private var lettered: Bool { draft.style.isLettered }

    private func refreshMenuBarSection() {
        draft = draft.normalized()
        styleControl.selectedSegment = ReadoutStyle.allCases.firstIndex(of: draft.style) ?? 0
        badgePopup.selectItem(at: BadgeMode.allCases.firstIndex(of: draft.badge) ?? 0)
        iconCheckbox.state = draft.showsAppIcon ? .on : .off
        let usesWindows = draft.style != .iconOnly
        table.isEnabled = usesWindows
        badgePopup.isEnabled = lettered
        iconCheckbox.isEnabled = lettered
        tableHeight?.constant = Self.rowHeight * CGFloat(draft.windows.count) + 2
        table.reloadData()
        updateAddRemove()
        let count = draft.windows.count
        var hint = count >= MenuBarReadout.maxWindows ? "Three is the most." : count <= 1 ? "At least one window." : "Up to three, from any provider."
        if draft.style == .percent { hint += " Percent shows the first." }
        windowsHint.stringValue = hint
        refreshPreviewAndWarnings()
    }

    private func updateAddRemove() {
        let usesWindows = draft.style != .iconOnly
        addRemove.setEnabled(usesWindows && draft.windows.count < MenuBarReadout.maxWindows && nextAvailableWindow() != nil, forSegment: 0)
        addRemove.setEnabled(usesWindows && draft.windows.count > 1 && table.selectedRow >= 0, forSegment: 1)
    }

    private func refreshPreviewAndWarnings() {
        let cells = state.cells(for: draft.windows)
        let images = [true, false].map { dark -> NSImage? in
            switch draft.style {
            case .iconOnly:
                return ReadoutRenderer.appIcon()
            case .percent:
                return ReadoutRenderer.percentImage(text: state.presentation(for: draft.windows.first).text)
            case .letteredPair, .splitPill:
                let options = ReadoutRenderer.Options(style: draft.style, badges: draft.badgeVisible, appIcon: draft.showsAppIcon,
                                                      dark: dark, increasedContrast: NSWorkspace.shared.accessibilityDisplayShouldIncreaseContrast)
                return ReadoutRenderer.image(cells: cells, options: options, marks: marks(for: cells))
            }
        }
        darkPreview.image = images[0]
        lightPreview.image = images[1]
        let width = images[0]?.size.width ?? 0
        previewWidth.stringValue = (width.rounded() == width ? String(Int(width)) : String(format: "%.1f", width)) + "pt wide"

        // Two chosen windows with the same letters look alike when they share a
        // provider (the badge can't help) or when no badge is drawn.
        if lettered, let clash = ReadoutLetters.clash(draft.windows, letters: cells.map(\.letter), badgeVisible: draft.badgeVisible) {
            let names = "\(Self.name(cells[clash.first])) and \(Self.name(cells[clash.second]))"
            let why = clash.sameProvider ? "They look the same in the menu bar whatever the badge setting."
                                         : "With badges off they look the same in the menu bar."
            clashWarning.show("\(names) both read \(cells[clash.first].letter). \(why)")
        } else {
            clashWarning.show(nil)
        }
        if draft.style != .iconOnly, !state.windows.isEmpty, let gone = cells.first(where: { !$0.available }) {
            goneWarning.show("\(Self.name(gone)) isn’t in your dashboard any more. It shows “—” until it comes back, or pick another window.")
        } else {
            goneWarning.show(nil)
        }
        grid?.row(at: 5).isHidden = clashWarning.message == nil
        grid?.row(at: 6).isHidden = goneWarning.message == nil
        resizeToFit()
    }

    /// Grows or shrinks the window with its content, keeping its top edge in place.
    private func resizeToFit() {
        guard let window, let content = window.contentView else { return }
        content.layoutSubtreeIfNeeded()
        let height = content.fittingSize.height
        guard height > 0, abs(height - content.frame.height) > 0.5 else { return }
        var frame = window.frameRect(forContentRect: NSRect(x: 0, y: 0, width: 490, height: height))
        frame.origin = NSPoint(x: window.frame.minX, y: window.frame.maxY - frame.height)
        window.setFrame(frame, display: window.isVisible)
    }

    private static func name(_ cell: ReadoutCell) -> String { cell.title.replacingOccurrences(of: " · ", with: " ") }

    private func marks(for cells: [ReadoutCell]) -> [String: LogoMark] {
        var marks: [String: LogoMark] = [:]
        for provider in Set(cells.map(\.selection.providerID)) { marks[provider] = logos.mark(for: provider) }
        return marks
    }

    /// Every window the summary lists, plus chosen ones it no longer has.
    private var choices: [QuotaWindow] { state.windows }

    private func nextAvailableWindow() -> QuotaSelection? {
        choices.map(\.selection).first { !draft.windows.contains($0) }
    }

    fileprivate func letter(for selection: QuotaSelection) -> String {
        ReadoutLetters.letter(for: selection, among: state.windows.map(\.selection))
    }

    fileprivate func title(for selection: QuotaSelection) -> String {
        state.cells(for: [selection]).first?.title ?? selection.rowID
    }

    /// The pop-up for one row: every window, grouped by provider under its logo,
    /// each with its letter. Windows in another row are disabled and say which.
    fileprivate func windowMenu(forRow row: Int) -> (NSMenu, NSMenuItem?) {
        let menu = NSMenu()
        menu.autoenablesItems = false
        let current = draft.windows[row]
        var selected: NSMenuItem?
        var lastProvider: String?
        var entries = choices.map { ($0.selection, $0.title) }
        if !entries.contains(where: { $0.0 == current }) { entries.append((current, title(for: current))) }
        for (selection, fullTitle) in entries {
            let parts = fullTitle.components(separatedBy: " · ")
            if selection.providerID != lastProvider {
                if lastProvider != nil { menu.addItem(.separator()) }
                let header = NSMenuItem(title: parts.count == 2 ? parts[0] : selection.providerID, action: nil, keyEquivalent: "")
                header.isEnabled = false
                header.attributedTitle = NSAttributedString(string: header.title, attributes: [
                    .font: NSFont.systemFont(ofSize: 11, weight: .semibold), .foregroundColor: NSColor.secondaryLabelColor])
                header.image = logos.image(for: selection.providerID, size: 12)
                menu.addItem(header)
                lastProvider = selection.providerID
            }
            let rowTitle = parts.count == 2 ? parts[1] : fullTitle
            let other = draft.windows.firstIndex(of: selection).flatMap { $0 == row ? nil : $0 }
            let gone = !choices.contains { $0.selection == selection }
            let item = NSMenuItem(title: fullTitle, action: nil, keyEquivalent: "")
            let text = NSMutableAttributedString(string: letter(for: selection), attributes: [
                .font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .bold)])
            var suffix = rowTitle
            if let other { suffix += " (in row \(other + 1))" }
            if gone { suffix += " (unavailable)" }
            text.append(NSAttributedString(string: "\t" + suffix, attributes: [.font: NSFont.menuFont(ofSize: 13)]))
            let paragraph = NSMutableParagraphStyle()
            paragraph.tabStops = [NSTextTab(textAlignment: .left, location: 32)]
            text.addAttribute(.paragraphStyle, value: paragraph, range: NSRange(location: 0, length: text.length))
            item.attributedTitle = text
            item.representedObject = selection
            item.isEnabled = other == nil
            menu.addItem(item)
            if selection == current { selected = item }
        }
        return (menu, selected)
    }

    fileprivate func choose(_ selection: QuotaSelection, forRow row: Int) {
        guard draft.windows.indices.contains(row), !draft.windows.contains(selection) || draft.windows[row] == selection else { return }
        draft.windows[row] = selection
        refreshMenuBarSection()
        table.selectRowIndexes(IndexSet(integer: row), byExtendingSelection: false)
    }

    // MARK: - Actions

    @objc private func styleChanged() {
        let index = styleControl.selectedSegment
        guard ReadoutStyle.allCases.indices.contains(index) else { return }
        draft.style = ReadoutStyle.allCases[index]
        refreshMenuBarSection()
    }

    @objc private func badgeChanged() {
        guard let raw = badgePopup.selectedItem?.representedObject as? String, let mode = BadgeMode(rawValue: raw) else { return }
        draft.badge = mode
        refreshPreviewAndWarnings()
    }

    @objc private func iconChanged() {
        draft.showsAppIcon = iconCheckbox.state == .on
        refreshPreviewAndWarnings()
    }

    @objc private func addOrRemove() {
        if addRemove.selectedSegment == 0 {
            guard draft.windows.count < MenuBarReadout.maxWindows, let next = nextAvailableWindow() else { return }
            draft.windows.append(next)
            refreshMenuBarSection()
            table.selectRowIndexes(IndexSet(integer: draft.windows.count - 1), byExtendingSelection: false)
        } else {
            let row = table.selectedRow
            guard draft.windows.count > 1, draft.windows.indices.contains(row) else { return }
            draft.windows.remove(at: row)
            refreshMenuBarSection()
        }
        updateAddRemove()
    }

    @objc private func saveSettings() {
        do {
            let location = try DashboardLocation(urlField.stringValue)
            // Always persisted; a different dashboard resets the windows and keeps the style.
            settings.save(location, readout: draft)
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

    // Test hooks.
    var draftForTesting: MenuBarReadout {
        get { draft }
        set { draft = newValue; refreshMenuBarSection() }
    }
    var clashMessageForTesting: String? { clashWarning.message }
    var goneMessageForTesting: String? { goneWarning.message }
}

// MARK: - Windows table

extension SettingsWindowController: NSTableViewDataSource, NSTableViewDelegate {
    func numberOfRows(in tableView: NSTableView) -> Int { draft.windows.count }

    func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
        guard draft.windows.indices.contains(row) else { return nil }
        let view = (tableView.makeView(withIdentifier: WindowRowView.identifier, owner: nil) as? WindowRowView) ?? WindowRowView()
        let selection = draft.windows[row]
        let (menu, selected) = windowMenu(forRow: row)
        view.configure(letter: letter(for: selection), logo: logos.image(for: selection.providerID, size: 14),
                       title: title(for: selection), available: state.windows.isEmpty || state.windows.contains { $0.selection == selection },
                       menu: menu, selected: selected, row: row, enabled: draft.style != .iconOnly)
        view.onChoose = { [weak self] choice, row in self?.choose(choice, forRow: row) }
        return view
    }

    func tableViewSelectionDidChange(_ notification: Notification) { updateAddRemove() }

    func tableView(_ tableView: NSTableView, pasteboardWriterForRow row: Int) -> NSPasteboardWriting? {
        guard draft.style != .iconOnly, draft.windows.count > 1 else { return nil }
        let item = NSPasteboardItem()
        item.setString(String(row), forType: Self.rowType)
        return item
    }

    func tableView(_ tableView: NSTableView, validateDrop info: NSDraggingInfo, proposedRow row: Int, proposedDropOperation dropOperation: NSTableView.DropOperation) -> NSDragOperation {
        guard info.draggingSource as? NSTableView === tableView else { return [] }
        if dropOperation == .on { tableView.setDropRow(row, dropOperation: .above) }
        return .move
    }

    func tableView(_ tableView: NSTableView, acceptDrop info: NSDraggingInfo, row: Int, dropOperation: NSTableView.DropOperation) -> Bool {
        guard let text = info.draggingPasteboard.pasteboardItems?.first?.string(forType: Self.rowType),
              let from = Int(text), draft.windows.indices.contains(from) else { return false }
        let moved = draft.windows.remove(at: from)
        let to = min(from < row ? row - 1 : row, draft.windows.count)
        draft.windows.insert(moved, at: to)
        refreshMenuBarSection()
        tableView.selectRowIndexes(IndexSet(integer: to), byExtendingSelection: false)
        return true
    }
}

/// One windows row: the letter it will draw, the provider's logo and a pop-up of every window.
@MainActor
private final class WindowRowView: NSTableCellView {
    static let identifier = NSUserInterfaceItemIdentifier("WindowRow")
    private let chip = NSTextField(labelWithString: "")
    private let logo = NSImageView()
    private let popup = NSPopUpButton(frame: .zero, pullsDown: false)
    private var row = 0
    var onChoose: ((QuotaSelection, Int) -> Void)?

    init() {
        super.init(frame: NSRect(x: 0, y: 0, width: 360, height: 26))
        identifier = Self.identifier
        chip.font = .monospacedDigitSystemFont(ofSize: 10.5, weight: .bold)
        chip.alignment = .center
        chip.wantsLayer = true
        chip.layer?.cornerRadius = 4
        chip.layer?.borderWidth = 1
        chip.layer?.borderColor = NSColor.separatorColor.cgColor
        logo.imageScaling = .scaleProportionallyUpOrDown
        logo.contentTintColor = .labelColor
        popup.isBordered = false
        popup.target = self
        popup.action = #selector(chosen)
        (popup.cell as? NSPopUpButtonCell)?.usesItemFromMenu = false
        for view in [chip, logo, popup] as [NSView] {
            view.translatesAutoresizingMaskIntoConstraints = false
            addSubview(view)
        }
        NSLayoutConstraint.activate([
            chip.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 7),
            chip.centerYAnchor.constraint(equalTo: centerYAnchor),
            chip.widthAnchor.constraint(equalToConstant: 26),
            chip.heightAnchor.constraint(equalToConstant: 16),
            logo.leadingAnchor.constraint(equalTo: chip.trailingAnchor, constant: 7),
            logo.centerYAnchor.constraint(equalTo: centerYAnchor),
            logo.widthAnchor.constraint(equalToConstant: 14),
            logo.heightAnchor.constraint(equalToConstant: 14),
            popup.leadingAnchor.constraint(equalTo: logo.trailingAnchor, constant: 4),
            popup.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -4),
            popup.centerYAnchor.constraint(equalTo: centerYAnchor),
        ])
    }

    required init?(coder: NSCoder) { fatalError("Not used") }

    func configure(letter: String, logo image: NSImage?, title: String, available: Bool, menu: NSMenu, selected: NSMenuItem?, row: Int, enabled: Bool) {
        self.row = row
        chip.stringValue = letter
        logo.image = image
        popup.menu = menu
        if let selected { popup.select(selected) }
        // The button shows "Provider · Window"; the menu shows letters and titles.
        let shown = NSMenuItem(title: available ? title : title + " (unavailable)", action: nil, keyEquivalent: "")
        (popup.cell as? NSPopUpButtonCell)?.menuItem = shown
        popup.isEnabled = enabled
        popup.setAccessibilityLabel("Window \(row + 1)")
        popup.setAccessibilityValue("\(letter), \(title)")
    }

    @objc private func chosen() {
        guard let selection = popup.selectedItem?.representedObject as? QuotaSelection else { return }
        onChoose?(selection, row)
    }
}

/// The readout drawn on a dark or a light bar, as the menu bar shows it.
@MainActor
private final class PreviewSwatch: NSView {
    private let imageView = NSImageView()
    var image: NSImage? {
        get { imageView.image }
        set { imageView.image = newValue; invalidateIntrinsicContentSize() }
    }

    init(dark: Bool) {
        super.init(frame: .zero)
        appearance = NSAppearance(named: dark ? .darkAqua : .aqua)
        wantsLayer = true
        layer?.cornerRadius = 5
        layer?.backgroundColor = dark ? NSColor(srgbRed: 0.149, green: 0.165, blue: 0.2, alpha: 1).cgColor
                                      : NSColor(srgbRed: 0.894, green: 0.902, blue: 0.925, alpha: 1).cgColor
        imageView.imageScaling = .scaleNone
        imageView.contentTintColor = dark ? .white : .black
        imageView.translatesAutoresizingMaskIntoConstraints = false
        addSubview(imageView)
        NSLayoutConstraint.activate([
            imageView.leadingAnchor.constraint(equalTo: leadingAnchor, constant: 6),
            imageView.trailingAnchor.constraint(equalTo: trailingAnchor, constant: -6),
            imageView.topAnchor.constraint(equalTo: topAnchor, constant: 1),
            imageView.bottomAnchor.constraint(equalTo: bottomAnchor, constant: -1),
        ])
        setAccessibilityElement(false)
    }

    required init?(coder: NSCoder) { fatalError("Not used") }
}

/// A yellow warning sign and one line of explanation, hidden when empty.
@MainActor
private final class WarningRow: NSStackView {
    private let text = NSTextField(wrappingLabelWithString: "")
    private(set) var message: String?

    init() {
        super.init(frame: .zero)
        let sign = NSImageView()
        sign.image = NSImage(systemSymbolName: "exclamationmark.triangle.fill", accessibilityDescription: "Warning")
        sign.contentTintColor = .systemYellow
        sign.setContentHuggingPriority(.required, for: .horizontal)
        text.font = .systemFont(ofSize: 11)
        text.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        addArrangedSubview(sign)
        addArrangedSubview(text)
        alignment = .top
        spacing = 6
        isHidden = true
    }

    required init?(coder: NSCoder) { fatalError("Not used") }

    func show(_ message: String?) {
        self.message = message
        text.stringValue = message ?? ""
        isHidden = message == nil
    }
}
