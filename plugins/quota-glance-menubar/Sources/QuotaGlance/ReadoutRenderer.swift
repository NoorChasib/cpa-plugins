import AppKit
import CoreText
import GlanceCore

/// Draws the Lettered pair and Split pill into one template image per update,
/// following the signed-off board (refined.html, "Building it"), and their
/// stacked siblings, the Stacked pair and Stacked pill (0.6.0), which put
/// each number first with its logo over its letter. One template image keeps
/// letters, numbers, badges and the pill's fill in one ink; the system
/// supplies white or black, the highlight and the inactive fade. The alphas
/// are baked in, so the image is redrawn when the appearance or Increase
/// Contrast changes.
enum ReadoutRenderer {
    /// In GlanceCore, so the status item's comparable value can hold it.
    typealias Options = ReadoutOptions

    /// The image is as tall as the menu bar's content area; the button centres it.
    static let height: CGFloat = 22

    /// A lettered style. `marks` maps provider IDs to their logos; a provider
    /// without one gets no badge.
    static func image(cells: [ReadoutCell], options: Options, marks: [String: LogoMark]) -> NSImage {
        let width = layout(cells: cells, options: options, marks: marks, scale: 2).width
        let image = NSImage(size: NSSize(width: width, height: height), flipped: false) { _ in
            guard let context = NSGraphicsContext.current?.cgContext else { return false }
            let scale = max(1, abs(context.userSpaceToDeviceSpaceTransform.a))
            Self.draw(Self.layout(cells: cells, options: options, marks: marks, scale: scale), in: context)
            return true
        }
        image.isTemplate = true
        return image
    }

    /// Today's Percent form (icon, then "59%"), for the Settings preview only;
    /// the status item itself keeps using the button title for it.
    static func percentImage(text: String) -> NSImage {
        let icon = appIcon
        let glyphs = Glyphs(text, font: .monospacedDigitSystemFont(ofSize: 12, weight: .medium), alpha: 1)
        let iconWidth = icon?.size.width ?? 0
        let width = (iconWidth + 8 + glyphs.advance).rounded(.up)
        let image = NSImage(size: NSSize(width: width, height: height), flipped: false) { _ in
            guard let context = NSGraphicsContext.current?.cgContext else { return false }
            if let icon {
                icon.draw(in: NSRect(x: 0, y: ((Self.height - icon.size.height) / 2).rounded(), width: icon.size.width, height: icon.size.height))
            }
            context.textMatrix = .identity
            context.textPosition = CGPoint(x: iconWidth + 8, y: ((Self.height - glyphs.capHeight) / 2).rounded())
            CTLineDraw(glyphs.line, context)
            return true
        }
        image.isTemplate = true
        return image
    }

    /// Created once: the status item, the drawn readout and the Settings
    /// preview share it.
    static let appIcon: NSImage? = {
        let icon = NSImage(systemSymbolName: "chart.bar.xaxis", accessibilityDescription: "Quota Glance")
        icon?.isTemplate = true
        return icon
    }()

    // MARK: - Layout

    struct Plan {
        var width: CGFloat
        var operations: [Operation]
    }

    enum Operation {
        case icon(NSImage, CGRect)
        /// Pill halves, clipped to the rounded box.
        case fill([CGRect], box: CGRect, radius: CGFloat, alpha: CGFloat)
        /// A line of text with its baseline origin.
        case text(CTLine, CGPoint)
        case logo(LogoMark, CGRect)
    }

    /// A measured line of text: advance, and ink side bearings from glyph paths.
    struct Glyphs {
        let line: CTLine
        let advance: CGFloat
        let leftBearing: CGFloat
        let rightBearing: CGFloat
        let capHeight: CGFloat

        init(_ text: String, font: NSFont, alpha: CGFloat) {
            let colorKey = NSAttributedString.Key(kCTForegroundColorAttributeName as String)
            let string = NSAttributedString(string: text, attributes: [.font: font, colorKey: CGColor(gray: 0, alpha: alpha)])
            line = CTLineCreateWithAttributedString(string)
            advance = CGFloat(CTLineGetTypographicBounds(line, nil, nil, nil))
            let ink = CTLineGetBoundsWithOptions(line, .useGlyphPathBounds)
            leftBearing = ink.isNull || ink.isEmpty ? 0 : ink.minX
            rightBearing = ink.isNull || ink.isEmpty ? 0 : advance - ink.maxX
            capHeight = font.capHeight
        }

        var inkWidth: CGFloat { advance - leftBearing - rightBearing }
    }

    private struct Fonts {
        let letter: NSFont
        let number: NSFont

        init(style: ReadoutStyle, increasedContrast: Bool) {
            if style == .splitPill {
                letter = .systemFont(ofSize: ReadoutMetrics.pillFontSize, weight: .semibold)
                number = .monospacedDigitSystemFont(ofSize: ReadoutMetrics.pillFontSize, weight: .bold)
            } else {
                // Increase Contrast: letters in full ink and semibold.
                letter = .monospacedDigitSystemFont(ofSize: ReadoutMetrics.pairFontSize, weight: increasedContrast ? .semibold : .medium)
                number = .monospacedDigitSystemFont(ofSize: ReadoutMetrics.pairFontSize, weight: .medium)
            }
        }
    }

    /// The optional app icon, and where the readout starts after it.
    private static func appIconPrefix(_ options: Options, scale: Double) -> (operations: [Operation], end: CGFloat) {
        guard options.appIcon, let icon = appIcon else { return ([], 0) }
        let size = icon.size
        let y = CGFloat(ReadoutLayout.snap(Double((height - size.height) / 2), scale: scale))
        return ([.icon(icon, CGRect(x: 0, y: y, width: size.width, height: size.height))],
                (size.width + CGFloat(ReadoutMetrics.appIconGap)).rounded(.up))
    }

    static func layout(cells: [ReadoutCell], options: Options, marks: [String: LogoMark], scale: CGFloat) -> Plan {
        if options.style.isStacked { return stackedLayout(cells: cells, options: options, marks: marks, scale: scale) }
        let pill = options.style == .splitPill
        let fonts = Fonts(style: options.style, increasedContrast: options.increasedContrast)
        let letterAlpha = CGFloat(ReadoutMetrics.letterAlpha(pill: pill, dark: options.dark, increasedContrast: options.increasedContrast))
        let s = Double(scale)
        func snap(_ value: CGFloat) -> CGFloat { CGFloat(ReadoutLayout.snap(Double(value), scale: s)) }
        let badge = CGFloat(ReadoutMetrics.badgeSize)
        let letterGap = CGFloat(ReadoutMetrics.letterGap)
        let badgeGap = CGFloat(ReadoutMetrics.badgeGap)

        var (operations, x) = appIconPrefix(options, scale: s)

        struct Cell {
            let letter: Glyphs
            let number: Glyphs
            let twoDigits: Glyphs
            let placement: Glyphs
            let mark: LogoMark?
        }
        let measured: [Cell] = cells.map { cell in
            let mark = options.badges ? marks[cell.selection.providerID] : nil
            let number = Glyphs(cell.text, font: fonts.number, alpha: 1)
            let twoDigits = Glyphs("00", font: fonts.number, alpha: 1)
            // Any two digits are placed as "00", so the letter never shifts with the reading.
            let placement = cell.text.count == 2 && cell.percent != nil ? twoDigits : number
            return Cell(letter: Glyphs(cell.letter, font: fonts.letter, alpha: letterAlpha), number: number,
                        twoDigits: twoDigits, placement: placement, mark: mark)
        }
        let capHeight = measured.first?.number.capHeight ?? fonts.number.capHeight
        let baseline = snap((height - capHeight) / 2)
        let badgeY = CGFloat(ReadoutLayout.badgeOriginY(baseline: Double(baseline), capHeight: Double(capHeight), scale: s))

        func drawRun(_ cell: Cell, at start: CGFloat) {
            operations.append(.text(cell.letter.line, CGPoint(x: start, y: baseline)))
            let numberX = start + cell.letter.advance + letterGap
            operations.append(.text(cell.number.line, CGPoint(x: numberX, y: baseline)))
            if let mark = cell.mark {
                operations.append(.logo(mark, CGRect(x: snap(numberX + cell.placement.advance + badgeGap), y: badgeY, width: badge, height: badge)))
            }
        }

        if pill {
            func run(_ cell: Cell, _ number: Glyphs) -> ReadoutRun {
                ReadoutRun(letterAdvance: Double(cell.letter.advance), letterLeftBearing: Double(cell.letter.leftBearing),
                           numberAdvance: Double(number.advance), numberRightBearing: Double(number.rightBearing),
                           badgeRightInset: cell.mark.map { $0.rightInkInset * ReadoutMetrics.badgeSize })
            }
            let half = CGFloat(ReadoutLayout.pillHalfWidth(twoDigit: measured.map { run($0, $0.twoDigits) },
                                                           shown: measured.map { run($0, $0.placement) }))
            let divider = CGFloat(ReadoutMetrics.pillDivider)
            let pillHeight = CGFloat(ReadoutMetrics.pillHeight)
            let box = CGRect(x: x, y: ((height - pillHeight) / 2).rounded(), width: CGFloat(ReadoutLayout.pillWidth(halfWidth: Double(half), halves: measured.count)), height: pillHeight)
            let halves = measured.indices.map { CGRect(x: x + CGFloat($0) * (half + divider), y: box.minY, width: half, height: pillHeight) }
            operations.append(.fill(halves, box: box, radius: CGFloat(ReadoutMetrics.pillRadius),
                                    alpha: CGFloat(ReadoutMetrics.pillFillAlpha(dark: options.dark, increasedContrast: options.increasedContrast))))
            for (cell, rect) in zip(measured, halves) {
                let origin = CGFloat(ReadoutLayout.pillContentOrigin(halfWidth: Double(half), run: run(cell, cell.placement), scale: s))
                drawRun(cell, at: rect.minX + origin)
            }
            x = box.maxX
        } else {
            // 6pt with badges, 7pt without: count the badges actually drawn, so
            // providers without a bundled logo space as if badges were off.
            let gap = CGFloat(ReadoutLayout.pairGap(badges: measured.contains { $0.mark != nil }))
            for (index, cell) in measured.enumerated() {
                if index > 0 { x += gap }
                let slot = CGFloat(ReadoutLayout.pairSlotWidth(letterAdvance: Double(cell.letter.advance), numberAdvance: Double(cell.number.advance),
                                                               twoDigitAdvance: Double(cell.twoDigits.advance), badge: cell.mark != nil))
                let content = cell.letter.advance + letterGap + cell.number.advance + (cell.mark == nil ? 0 : badgeGap + badge)
                // Spare room sits before the letter, never between letter and number.
                drawRun(cell, at: snap(x + slot - content))
                x += slot
            }
        }
        return Plan(width: max(1, x.rounded(.up)), operations: operations)
    }

    /// Stacked pair or Stacked pill: each reading is its number, then one
    /// column with the logo on top and the letter under it, the column
    /// centred on the digits' cap height. Without a logo the letter stands
    /// alone, centred the same way.
    private static func stackedLayout(cells: [ReadoutCell], options: Options, marks: [String: LogoMark], scale: CGFloat) -> Plan {
        let pill = options.style == .stackedPill
        let numberFont = NSFont.monospacedDigitSystemFont(ofSize: CGFloat(ReadoutMetrics.stackedFontSize), weight: pill ? .bold : .medium)
        let letterFont = NSFont.systemFont(ofSize: CGFloat(ReadoutMetrics.stackLetterSize), weight: .semibold)
        let letterAlpha = CGFloat(ReadoutMetrics.letterAlpha(pill: pill, dark: options.dark, increasedContrast: options.increasedContrast))
        let s = Double(scale)
        func snap(_ value: CGFloat) -> CGFloat { CGFloat(ReadoutLayout.snap(Double(value), scale: s)) }
        let logo = CGFloat(ReadoutMetrics.stackLogoSize)
        let numberGap = CGFloat(ReadoutMetrics.stackNumberGap)

        var (operations, x) = appIconPrefix(options, scale: s)

        struct Cell {
            let letter: Glyphs
            let number: Glyphs
            let twoDigits: Glyphs
            let placement: Glyphs
            let mark: LogoMark?

            func run(_ number: Glyphs) -> StackedRun {
                StackedRun(numberAdvance: Double(number.advance), numberLeftBearing: Double(number.leftBearing),
                           letterInkWidth: Double(letter.inkWidth),
                           logoRightInset: mark.map { $0.rightInkInset * ReadoutMetrics.stackLogoSize })
            }
        }
        let twoDigits = Glyphs("00", font: numberFont, alpha: 1)
        let measured: [Cell] = cells.map { cell in
            let number = Glyphs(cell.text, font: numberFont, alpha: 1)
            // Any two digits are placed as "00", so the stack never shifts with the reading.
            let placement = cell.text.count == 2 && cell.percent != nil ? twoDigits : number
            return Cell(letter: Glyphs(cell.letter, font: letterFont, alpha: letterAlpha), number: number, twoDigits: twoDigits,
                        placement: placement, mark: options.badges ? marks[cell.selection.providerID] : nil)
        }
        let capHeight = numberFont.capHeight
        let baseline = snap((height - capHeight) / 2)
        // The digits' cap-height centre: the bar's centre, which the stack shares.
        let centre = Double(baseline + capHeight / 2)

        // `start` need not be on the pixel grid: the number and each part of
        // the stack are snapped on their own, so the stack keeps its place.
        func drawRun(_ cell: Cell, at start: CGFloat) {
            operations.append(.text(cell.number.line, CGPoint(x: snap(start), y: baseline)))
            let column = start + cell.placement.advance + numberGap
            let width = CGFloat(cell.run(cell.placement).stackWidth)
            let place = ReadoutLayout.stackPlacement(centre: centre, letterCapHeight: Double(cell.letter.capHeight),
                                                     logo: cell.mark != nil, scale: s)
            // Letter and logo are each centred in the column, the letter by its ink.
            let letterX = snap(column + (width - cell.letter.inkWidth) / 2 - cell.letter.leftBearing)
            operations.append(.text(cell.letter.line, CGPoint(x: letterX, y: CGFloat(place.letterBaseline))))
            if let mark = cell.mark, let logoY = place.logoOriginY {
                operations.append(.logo(mark, CGRect(x: snap(column + (width - logo) / 2), y: CGFloat(logoY), width: logo, height: logo)))
            }
        }

        if pill {
            let half = CGFloat(ReadoutLayout.pillHalfWidth(twoDigit: measured.map { $0.run($0.twoDigits) },
                                                           shown: measured.map { $0.run($0.placement) },
                                                           padding: ReadoutMetrics.stackedPillPadding))
            let divider = CGFloat(ReadoutMetrics.pillDivider)
            let pillHeight = CGFloat(ReadoutMetrics.stackedPillHeight)
            let box = CGRect(x: x, y: ((height - pillHeight) / 2).rounded(), width: CGFloat(ReadoutLayout.pillWidth(halfWidth: Double(half), halves: measured.count)), height: pillHeight)
            let halves = measured.indices.map { CGRect(x: x + CGFloat($0) * (half + divider), y: box.minY, width: half, height: pillHeight) }
            operations.append(.fill(halves, box: box, radius: CGFloat(ReadoutMetrics.stackedPillRadius),
                                    alpha: CGFloat(ReadoutMetrics.pillFillAlpha(dark: options.dark, increasedContrast: options.increasedContrast))))
            for (cell, rect) in zip(measured, halves) {
                let origin = CGFloat(ReadoutLayout.pillContentOrigin(halfWidth: Double(half), run: cell.run(cell.placement), scale: s))
                drawRun(cell, at: rect.minX + origin)
            }
            x = box.maxX
        } else {
            let gap = CGFloat(ReadoutMetrics.stackedPairSlotGap)
            for (index, cell) in measured.enumerated() {
                if index > 0 { x += gap }
                let run = cell.run(cell.placement)
                let slot = CGFloat(ReadoutLayout.stackedSlotWidth(numberAdvance: Double(cell.number.advance), twoDigitAdvance: Double(twoDigits.advance),
                                                                  stackWidth: run.stackWidth))
                // Spare room sits before the number, never between number and stack.
                drawRun(cell, at: CGFloat(ReadoutLayout.stackedContentOrigin(slotStart: Double(x), slotWidth: Double(slot), run: run)))
                x += slot
            }
        }
        return Plan(width: max(1, x.rounded(.up)), operations: operations)
    }

    static func draw(_ plan: Plan, in context: CGContext) {
        for operation in plan.operations {
            switch operation {
            case let .icon(image, rect):
                image.draw(in: rect)
            case let .fill(rects, box, radius, alpha):
                context.saveGState()
                context.addPath(CGPath(roundedRect: box, cornerWidth: radius, cornerHeight: radius, transform: nil))
                context.clip()
                context.setFillColor(CGColor(gray: 0, alpha: alpha))
                context.fill(rects)
                context.restoreGState()
            case let .text(line, origin):
                context.saveGState()
                context.textMatrix = .identity
                context.textPosition = origin
                CTLineDraw(line, context)
                context.restoreGState()
            case let .logo(mark, rect):
                ProviderLogos.draw(mark, in: rect, context: context)
            }
        }
    }
}
