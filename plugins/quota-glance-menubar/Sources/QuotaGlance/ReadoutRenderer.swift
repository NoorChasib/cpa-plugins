import AppKit
import CoreText
import GlanceCore

/// Draws the Lettered pair and Split pill into one template image per update,
/// following the signed-off board (refined.html, "Building it"). One template
/// image keeps letters, numbers, badges and the pill's fill in one ink; the
/// system supplies white or black, the highlight and the inactive fade. The
/// alphas are baked in, so the image is redrawn when the appearance or
/// Increase Contrast changes.
enum ReadoutRenderer {
    struct Options: Equatable {
        var style: ReadoutStyle
        var badges: Bool
        var appIcon: Bool
        var dark: Bool
        var increasedContrast: Bool
    }

    /// The image is as tall as the menu bar's content area; the button centres it.
    static let height: CGFloat = 22

    /// Lettered pair or Split pill. `marks` maps provider IDs to their logos;
    /// a provider without one gets no badge.
    static func image(cells: [ReadoutCell], options: Options, marks: [String: LogoMark]) -> NSImage {
        let width = layout(cells: cells, options: options, marks: marks, scale: 2).width
        let image = NSImage(size: NSSize(width: width, height: height), flipped: false) { _ in
            guard let context = NSGraphicsContext.current?.cgContext else { return false }
            let scale = max(1, abs(context.userSpaceToDeviceSpaceTransform.a))
            draw(layout(cells: cells, options: options, marks: marks, scale: scale), in: context)
            return true
        }
        image.isTemplate = true
        return image
    }

    /// Today's Percent form (icon, then "59%"), for the Settings preview only;
    /// the status item itself keeps using the button title for it.
    static func percentImage(text: String) -> NSImage {
        let icon = appIcon()
        let glyphs = Glyphs(text, font: .monospacedDigitSystemFont(ofSize: 12, weight: .medium), alpha: 1)
        let iconWidth = icon?.size.width ?? 0
        let width = (iconWidth + 8 + glyphs.advance).rounded(.up)
        let image = NSImage(size: NSSize(width: width, height: height), flipped: false) { _ in
            guard let context = NSGraphicsContext.current?.cgContext else { return false }
            if let icon {
                icon.draw(in: NSRect(x: 0, y: ((height - icon.size.height) / 2).rounded(), width: icon.size.width, height: icon.size.height))
            }
            context.textMatrix = .identity
            context.textPosition = CGPoint(x: iconWidth + 8, y: ((height - glyphs.capHeight) / 2).rounded())
            CTLineDraw(glyphs.line, context)
            return true
        }
        image.isTemplate = true
        return image
    }

    static func appIcon() -> NSImage? {
        let icon = NSImage(systemSymbolName: "chart.bar.xaxis", accessibilityDescription: "Quota Glance")
        icon?.isTemplate = true
        return icon
    }

    // MARK: - Layout

    struct Plan {
        var width: CGFloat
        var operations: [Operation]
    }

    enum Operation {
        case icon(NSImage, CGRect)
        /// Split pill halves, clipped to the rounded box.
        case fill([CGRect], box: CGRect, alpha: CGFloat)
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

    static func layout(cells: [ReadoutCell], options: Options, marks: [String: LogoMark], scale: CGFloat) -> Plan {
        let pill = options.style == .splitPill
        let fonts = Fonts(style: options.style, increasedContrast: options.increasedContrast)
        let letterAlpha = CGFloat(ReadoutMetrics.letterAlpha(pill: pill, dark: options.dark, increasedContrast: options.increasedContrast))
        let s = Double(scale)
        func snap(_ value: CGFloat) -> CGFloat { CGFloat(ReadoutLayout.snap(Double(value), scale: s)) }
        let badge = CGFloat(ReadoutMetrics.badgeSize)
        let letterGap = CGFloat(ReadoutMetrics.letterGap)
        let badgeGap = CGFloat(ReadoutMetrics.badgeGap)

        var operations: [Operation] = []
        var x: CGFloat = 0
        if options.appIcon, let icon = appIcon() {
            let size = icon.size
            operations.append(.icon(icon, CGRect(x: 0, y: snap((height - size.height) / 2), width: size.width, height: size.height)))
            x = (size.width + CGFloat(ReadoutMetrics.appIconGap)).rounded(.up)
        }

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
            operations.append(.fill(halves, box: box, alpha: CGFloat(ReadoutMetrics.pillFillAlpha(dark: options.dark, increasedContrast: options.increasedContrast))))
            for (cell, rect) in zip(measured, halves) {
                let origin = CGFloat(ReadoutLayout.pillContentOrigin(halfWidth: Double(half), run: run(cell, cell.placement), scale: s))
                drawRun(cell, at: rect.minX + origin)
            }
            x = box.maxX
        } else {
            let gap = CGFloat(ReadoutLayout.pairGap(badges: options.badges))
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

    static func draw(_ plan: Plan, in context: CGContext) {
        for operation in plan.operations {
            switch operation {
            case let .icon(image, rect):
                image.draw(in: rect)
            case let .fill(rects, box, alpha):
                context.saveGState()
                context.addPath(CGPath(roundedRect: box, cornerWidth: CGFloat(ReadoutMetrics.pillRadius), cornerHeight: CGFloat(ReadoutMetrics.pillRadius), transform: nil))
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
