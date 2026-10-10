import AppKit
import CoreText
import GlanceCore
import XCTest
@testable import QuotaGlance

final class ReadoutRendererTests: XCTestCase {
    private let logos = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().appendingPathComponent("Resources/Logos")

    private func marks() throws -> [String: LogoMark] {
        var marks: [String: LogoMark] = [:]
        for provider in ["claude", "codex", "xai"] {
            marks[provider] = try LogoMark(contentsOf: logos.appendingPathComponent("\(provider).svg"))
        }
        return marks
    }

    private func cells(_ readings: [Int?], providers: [String] = ["claude", "claude"]) -> [ReadoutCell] {
        let rows = ["session", "weekly", "weekly_fable"]
        return readings.enumerated().map { index, percent in
            ReadoutCell(selection: QuotaSelection(providerID: providers[index], rowID: rows[index]),
                        letter: ["S", "W", "F"][index], percent: percent, title: "Claude · Row", available: true)
        }
    }

    /// Every style drawn by ReadoutRenderer.
    private let lettered = ReadoutStyle.allCases.filter(\.isLettered)
    private let stackedStyles = [ReadoutStyle.stackedPair, .stackedPill]

    /// S97 and W29 from Claude, W34 from Codex: the readout the stacked styles were designed on.
    private func designCells(_ readings: [Int?] = [97, 29, 34]) -> [ReadoutCell] {
        let rows = [("claude", "session", "S", "Claude · Session"), ("claude", "weekly", "W", "Claude · Weekly"),
                    ("codex", "weekly", "W", "Codex · Weekly")]
        return zip(rows, readings).map { row, percent in
            ReadoutCell(selection: QuotaSelection(providerID: row.0, rowID: row.1), letter: row.2, percent: percent,
                        title: row.3, available: true)
        }
    }

    private func options(_ style: ReadoutStyle, badges: Bool = true, icon: Bool = false, dark: Bool = true, contrast: Bool = false) -> ReadoutRenderer.Options {
        ReadoutRenderer.Options(style: style, badges: badges, appIcon: icon, dark: dark, increasedContrast: contrast)
    }

    /// The image's pixels at 2x, so drawings can be compared.
    private func pixels(_ image: NSImage) throws -> Data {
        let width = Int(image.size.width * 2), height = Int(image.size.height * 2)
        let rep = try XCTUnwrap(NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: width, pixelsHigh: height, bitsPerSample: 8,
                                                 samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                                                 bytesPerRow: width * 4, bitsPerPixel: 32))
        rep.size = image.size
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
        image.draw(in: NSRect(origin: .zero, size: image.size))
        NSGraphicsContext.restoreGraphicsState()
        let bytes = try XCTUnwrap(rep.bitmapData)
        return Data(bytes: bytes, count: width * height * 4)
    }

    func testBothStylesAreTemplateImagesOfWholePointWidth() throws {
        for style in lettered {
            let image = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style), marks: try marks())
            XCTAssertTrue(image.isTemplate, "\(style)")
            XCTAssertEqual(image.size.height, ReadoutRenderer.height)
            XCTAssertEqual(image.size.width, image.size.width.rounded(), "\(style)")
            XCTAssertGreaterThan(image.size.width, 40, "\(style)")
            XCTAssertFalse(try pixels(image).allSatisfy { $0 == 0 }, "\(style) draws something")
        }
    }

    func testTwoDigitsOneDigitAndDashKeepTheWidth() throws {
        for style in lettered {
            let widths = [[94, 59], [6, 3], [nil, nil], [0, 0]].map {
                ReadoutRenderer.image(cells: cells($0), options: options(style), marks: (try? marks()) ?? [:]).size.width
            }
            XCTAssertEqual(Set(widths).count, 1, "\(style): \(widths)")
        }
    }

    func testSplitPillFitsAHundredWithoutChangingWidth() throws {
        let normal = ReadoutRenderer.image(cells: cells([94, 59]), options: options(.splitPill), marks: try marks()).size.width
        let hundred = ReadoutRenderer.image(cells: cells([100, 100]), options: options(.splitPill), marks: try marks()).size.width
        XCTAssertEqual(hundred, normal)
        let pair = ReadoutRenderer.image(cells: cells([94, 59]), options: options(.letteredPair), marks: try marks()).size.width
        let pair100 = ReadoutRenderer.image(cells: cells([100, 59]), options: options(.letteredPair), marks: try marks()).size.width
        XCTAssertGreaterThan(pair100, pair, "Lettered pair widens by one digit at 100")
    }

    func testBadgesIconAndThreeWindowsChangeTheWidth() throws {
        let m = try marks()
        for style in lettered {
            let on = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style), marks: m).size.width
            let off = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, badges: false), marks: m).size.width
            let icon = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, icon: true), marks: m).size.width
            let three = ReadoutRenderer.image(cells: cells([94, 59, 36], providers: ["claude", "claude", "codex"]), options: options(style), marks: m).size.width
            // A Stacked pill's halves are rounded to whole points, and its logo
            // column is barely wider than W alone, so badges off may round to
            // the same width. It is never wider.
            if style == .stackedPill {
                XCTAssertLessThanOrEqual(off, on, "\(style)")
            } else {
                XCTAssertLessThan(off, on, "\(style)")
            }
            XCTAssertGreaterThan(icon, on, "\(style)")
            XCTAssertGreaterThan(three, on, "\(style)")
        }
        // A provider without a logo draws no badge, so it is laid out exactly as
        // with badges off: no badge room and slots 7pt apart, not 6pt.
        let unknown = ReadoutRenderer.image(cells: cells([94, 59], providers: ["mystery", "mystery"]), options: options(.letteredPair), marks: m).size.width
        let off = ReadoutRenderer.image(cells: cells([94, 59]), options: options(.letteredPair, badges: false), marks: m).size.width
        XCTAssertEqual(unknown, off, "Slots are 6pt apart only when a badge is drawn")
        let plan = ReadoutRenderer.layout(cells: cells([94, 59], providers: ["mystery", "mystery"]), options: options(.letteredPair), marks: m, scale: 2)
        XCTAssertFalse(plan.operations.contains { if case .logo = $0 { return true } else { return false } })
    }

    func testLightDarkAndIncreasedContrastDrawDifferently() throws {
        let m = try marks()
        for style in lettered {
            let dark = try pixels(ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, dark: true), marks: m))
            let light = try pixels(ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, dark: false), marks: m))
            let contrast = try pixels(ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, dark: true, contrast: true), marks: m))
            XCTAssertNotEqual(dark, light, "\(style)")
            XCTAssertNotEqual(dark, contrast, "\(style)")
        }
    }

    /// Every alpha the plan paints with, in order: the pill fill and each text run's colour.
    private func inks(_ plan: ReadoutRenderer.Plan) -> [CGFloat] {
        plan.operations.flatMap { operation -> [CGFloat] in
            switch operation {
            case let .fill(_, _, _, alpha):
                return [alpha]
            case let .text(line, _):
                let runs = CTLineGetGlyphRuns(line) as? [CTRun] ?? []
                return runs.map { run in
                    let attributes = CTRunGetAttributes(run) as NSDictionary
                    guard let value = attributes[kCTForegroundColorAttributeName as String] else { return -1 }
                    return (value as! CGColor).alpha
                }
            case .icon, .logo:
                return []
            }
        }
    }

    func testLowReadingsUseTheSameInk() throws {
        // One ink at every level: no colour change and no dimming for low
        // readings, 0 or "—". The plan's fill and text alphas must not depend on the reading.
        let m = try marks()
        for style in lettered {
            for dark in [true, false] {
                let o = options(style, dark: dark)
                let high = inks(ReadoutRenderer.layout(cells: cells([94, 94]), options: o, marks: m, scale: 2))
                XCTAssertFalse(high.isEmpty)
                XCTAssertFalse(high.contains(-1), "Every text run carries its ink")
                for low in [[3, 3], [0, 0], [nil, nil]] as [[Int?]] {
                    let ink = inks(ReadoutRenderer.layout(cells: cells(low), options: o, marks: m, scale: 2))
                    XCTAssertEqual(ink, high, "\(style) dark=\(dark) \(low)")
                }
            }
        }
        // And in pixels, with badges off so only letters, numbers and fill draw:
        // the strongest ink and the fill beside the glyphs are unchanged.
        let high = ReadoutRenderer.image(cells: cells([94, 94]), options: options(.splitPill, badges: false), marks: m)
        let low = ReadoutRenderer.image(cells: cells([3, 3]), options: options(.splitPill, badges: false), marks: m)
        XCTAssertEqual(high.size, low.size)
        let highPixels = try pixels(high), lowPixels = try pixels(low)
        let strongest = { (data: Data) in stride(from: 3, to: data.count, by: 4).map { data[$0] }.max() }
        XCTAssertEqual(strongest(highPixels), strongest(lowPixels))
        // 2pt in from the pill's left edge, halfway up: fill only.
        let fill = (Int(high.size.height) * Int(high.size.width) * 2 + 4) * 4 + 3
        XCTAssertGreaterThan(highPixels[fill], 0)
        XCTAssertEqual(highPixels[fill], lowPixels[fill])
    }

    func testAppIconIsOneSharedTemplateImage() throws {
        // Created once, not on every status item update.
        let icon = try XCTUnwrap(ReadoutRenderer.appIcon)
        XCTAssertTrue(icon === ReadoutRenderer.appIcon)
        XCTAssertTrue(icon.isTemplate)
        XCTAssertTrue(ReadoutRenderer.percentImage(text: "59%").isTemplate)
    }

    @MainActor
    func testBundledLogosLoadFromADirectoryAndRejectPaths() {
        let provider = ProviderLogos(directory: logos)
        XCTAssertNotNil(provider.mark(for: "claude"))
        XCTAssertNotNil(provider.image(for: "codex", size: 14))
        XCTAssertNil(provider.mark(for: "../Logos/claude"))
        XCTAssertNil(provider.mark(for: "openrouter"))
        XCTAssertNil(ProviderLogos(directory: nil).mark(for: "claude"))
    }

    // MARK: - Stacked pair and Stacked pill

    private func logoRects(_ plan: ReadoutRenderer.Plan) -> [CGRect] {
        plan.operations.compactMap { if case let .logo(_, rect) = $0 { return rect } else { return nil } }
    }

    /// Text runs in drawing order; the stacked styles draw each cell's number, then its letter.
    private func texts(_ plan: ReadoutRenderer.Plan) -> [(line: CTLine, origin: CGPoint)] {
        plan.operations.compactMap { if case let .text(line, origin) = $0 { return (line, origin) } else { return nil } }
    }

    private func pill(_ plan: ReadoutRenderer.Plan) -> (halves: [CGRect], box: CGRect, radius: CGFloat)? {
        for operation in plan.operations {
            if case let .fill(halves, box, radius, _) = operation { return (halves, box, radius) }
        }
        return nil
    }

    func testStackedStylesAreWholePointsAtEveryScale() throws {
        let m = try marks()
        for style in stackedStyles {
            for scale in [1, 2] as [CGFloat] {
                let plan = ReadoutRenderer.layout(cells: designCells(), options: options(style), marks: m, scale: scale)
                XCTAssertEqual(plan.width, plan.width.rounded(), "\(style) @\(scale)x")
                XCTAssertEqual(logoRects(plan).count, 3, "\(style): a logo per cell")
            }
            let image = ReadoutRenderer.image(cells: designCells(), options: options(style), marks: m)
            XCTAssertTrue(image.isTemplate, "\(style)")
            XCTAssertEqual(image.size.height, ReadoutRenderer.height)
        }
    }

    func testStackedPairKeepsEachStackInPlaceAtEveryReading() throws {
        let m = try marks()
        let readings: [[Int?]] = [[97, 29, 34], [6, 3, 1], [nil, nil, nil], [0, 59, nil]]
        let plans = readings.map { ReadoutRenderer.layout(cells: designCells($0), options: options(.stackedPair), marks: m, scale: 2) }
        XCTAssertEqual(Set(plans.map(\.width)).count, 1, "\(plans.map(\.width))")
        XCTAssertEqual(Set(plans.map { logoRects($0).map(\.minX) }).count, 1, "The logos never move with the reading")
        let letterXs = plans.map { plan in texts(plan).enumerated().filter { item in item.offset % 2 == 1 }.map { item in item.element.origin.x } }
        XCTAssertEqual(Set(letterXs).count, 1, "Nor do the letters")
        // A 100 widens its own slot only: the slots after it move by the same amount.
        let hundred = ReadoutRenderer.layout(cells: designCells([100, 29, 34]), options: options(.stackedPair), marks: m, scale: 2)
        let grown = hundred.width - plans[0].width
        XCTAssertGreaterThan(grown, 0)
        XCTAssertEqual(logoRects(hundred)[2].minX - logoRects(plans[0])[2].minX, grown)
    }

    func testStackedPillFitsAHundredByTheSplitPillsRule() throws {
        let m = try marks()
        let normalPlan = ReadoutRenderer.layout(cells: designCells(), options: options(.stackedPill), marks: m, scale: 2)
        let hundredPlan = ReadoutRenderer.layout(cells: designCells([100, 29, 34]), options: options(.stackedPill), marks: m, scale: 2)
        let normal = try XCTUnwrap(pill(normalPlan)), hundred = try XCTUnwrap(pill(hundredPlan))
        XCTAssertEqual(normal.box.height, CGFloat(ReadoutMetrics.stackedPillHeight))
        XCTAssertEqual(normal.radius, CGFloat(ReadoutMetrics.stackedPillRadius))
        XCTAssertEqual(normal.box.minY, 1, "Centred in the 22pt image")
        for value in [normal, hundred] {
            XCTAssertEqual(Set(value.halves.map(\.width)).count, 1, "Every half is the same width")
            XCTAssertEqual(value.halves[0].width, value.halves[0].width.rounded())
        }
        // The 100's ink keeps at least the minimum room at each edge of its half.
        let half = hundred.halves[0]
        let number = try XCTUnwrap(texts(hundredPlan).first)
        let numberInk = CTLineGetBoundsWithOptions(number.line, .useGlyphPathBounds)
        let room = CGFloat(ReadoutMetrics.pillMinimumRoom)
        XCTAssertGreaterThanOrEqual(number.origin.x + numberInk.minX - half.minX, room - 0.5)
        let logo = logoRects(hundredPlan)[0]
        let claude = try XCTUnwrap(m["claude"])
        let logoInkRight = logo.maxX - CGFloat(claude.rightInkInset) * logo.width
        XCTAssertLessThanOrEqual(logoInkRight, half.maxX - room + 0.5)
        // Every half grows alike (or none does), so the pill keeps equal halves.
        XCTAssertEqual(hundredPlan.width - normalPlan.width, 3 * (hundred.halves[0].width - normal.halves[0].width))
        // And it stays put for one digit and a dash.
        for low in [[6, 3, 1], [nil, nil, nil]] as [[Int?]] {
            let plan = ReadoutRenderer.layout(cells: designCells(low), options: options(.stackedPill), marks: m, scale: 2)
            XCTAssertEqual(plan.width, normalPlan.width, "\(low)")
        }
    }

    func testStackedBadgesOffDrawTheLetterAloneCentredOnTheDigits() throws {
        let m = try marks()
        for style in stackedStyles {
            let on = ReadoutRenderer.layout(cells: designCells(), options: options(style), marks: m, scale: 2)
            let off = ReadoutRenderer.layout(cells: designCells(), options: options(style, badges: false), marks: m, scale: 2)
            XCTAssertTrue(logoRects(off).isEmpty, "\(style)")
            // Never wider; the pill's whole-point halves can round it to the same width.
            if style == .stackedPill {
                XCTAssertLessThanOrEqual(off.width, on.width, "\(style)")
            } else {
                XCTAssertLessThan(off.width, on.width, "\(style): the letter alone is narrower than the logo")
            }
            // Under a logo the letter sits below the digits' centre; alone it is centred on it.
            let letterOn = texts(on)[1].origin.y, letterOff = texts(off)[1].origin.y
            XCTAssertGreaterThan(letterOff, letterOn, "\(style)")
            let font = NSFont.systemFont(ofSize: CGFloat(ReadoutMetrics.stackLetterSize), weight: .semibold)
            let digits = NSFont.monospacedDigitSystemFont(ofSize: CGFloat(ReadoutMetrics.stackedFontSize), weight: .medium)
            let centre = texts(off)[0].origin.y + digits.capHeight / 2
            XCTAssertEqual(letterOff + font.capHeight / 2, centre, accuracy: 0.5, "\(style)")
            // A provider without a bundled logo stacks its letter alone, too.
            let unknown = ReadoutRenderer.layout(cells: cells([97, 29], providers: ["mystery", "mystery"]), options: options(style), marks: m, scale: 2)
            let plain = ReadoutRenderer.layout(cells: cells([97, 29]), options: options(style, badges: false), marks: m, scale: 2)
            XCTAssertTrue(logoRects(unknown).isEmpty, "\(style)")
            XCTAssertEqual(unknown.width, plain.width, "\(style)")
        }
    }

    func testStackedStylesLightDarkAndIncreasedContrastDrawDifferently() throws {
        let m = try marks()
        for style in stackedStyles {
            let dark = try pixels(ReadoutRenderer.image(cells: designCells(), options: options(style, dark: true), marks: m))
            let light = try pixels(ReadoutRenderer.image(cells: designCells(), options: options(style, dark: false), marks: m))
            let contrast = try pixels(ReadoutRenderer.image(cells: designCells(), options: options(style, dark: true, contrast: true), marks: m))
            XCTAssertNotEqual(dark, light, "\(style)")
            XCTAssertNotEqual(dark, contrast, "\(style)")
            // The app icon goes before the readout, as for the other lettered styles.
            let icon = ReadoutRenderer.layout(cells: designCells(), options: options(style, icon: true), marks: m, scale: 2)
            guard case .icon = icon.operations.first else { return XCTFail("\(style): the icon comes first") }
        }
    }

    func testStackedPairIsNarrowerThanLetteredPair() throws {
        // The board's mockup measured about 101pt against 122pt for these three.
        let m = try marks()
        let letteredWidth = ReadoutRenderer.image(cells: designCells(), options: options(.letteredPair), marks: m).size.width
        let stackedWidth = ReadoutRenderer.image(cells: designCells(), options: options(.stackedPair), marks: m).size.width
        XCTAssertLessThan(stackedWidth, letteredWidth)
    }

    // MARK: - Renders for review

    /// Writes the real renderer's output as PNGs for a person to look at, only
    /// when READOUT_RENDER_DIR is set (CI uploads them as "readout-renders").
    /// Nobody can run AppKit where the stacked styles were written, so these
    /// are how their drawing is checked by eye.
    func testWriteReadoutRendersForReview() throws {
        guard let path = ProcessInfo.processInfo.environment["READOUT_RENDER_DIR"], !path.isEmpty else {
            throw XCTSkip("Set READOUT_RENDER_DIR to write readout renders")
        }
        let directory = URL(fileURLWithPath: path, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let m = try marks()
        func write(_ name: String, _ cells: [ReadoutCell], _ style: ReadoutStyle, dark: Bool, badges: Bool, scale: Int) throws {
            let image = ReadoutRenderer.image(cells: cells, options: options(style, badges: badges, dark: dark), marks: m)
            try composite(image, dark: dark, scale: scale).write(to: directory.appendingPathComponent("\(name)@\(scale)x.png"))
        }
        for style in [ReadoutStyle.letteredPair, .stackedPair, .splitPill, .stackedPill] {
            for dark in [true, false] {
                for badges in [true, false] {
                    let name = "\(style.rawValue)-\(dark ? "dark" : "light")-\(badges ? "badges" : "nobadges")"
                    try write(name, designCells(), style, dark: dark, badges: badges, scale: 2)
                    if dark && badges { try write(name, designCells(), style, dark: dark, badges: badges, scale: 1) }
                }
            }
        }
        // The first reading as 6, "—" and 100; the other two as before.
        for style in stackedStyles {
            for (label, reading) in [("6", 6), ("dash", nil), ("100", 100)] as [(String, Int?)] {
                try write("\(style.rawValue)-dark-badges-\(label)", designCells([reading, 29, 34]), style, dark: true, badges: true, scale: 2)
            }
        }
    }

    /// The template image as a menu bar shows it: its alpha masks white or
    /// black ink over an opaque bar colour, with 6px of bar around it.
    private func composite(_ image: NSImage, dark: Bool, scale: Int) throws -> Data {
        let pad = 6
        let width = Int(image.size.width) * scale + 2 * pad, height = Int(image.size.height) * scale + 2 * pad
        let space = try XCTUnwrap(CGColorSpace(name: CGColorSpace.sRGB))
        let context = try XCTUnwrap(CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                                              space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
        let all = CGRect(x: 0, y: 0, width: width, height: height)
        // #1d2430 or #ece8e2.
        context.setFillColor(dark ? CGColor(srgbRed: 0x1d / 255, green: 0x24 / 255, blue: 0x30 / 255, alpha: 1)
                                  : CGColor(srgbRed: 0xec / 255, green: 0xe8 / 255, blue: 0xe2 / 255, alpha: 1))
        context.fill(all)
        context.beginTransparencyLayer(auxiliaryInfo: nil)
        context.saveGState()
        context.translateBy(x: CGFloat(pad), y: CGFloat(pad))
        context.scaleBy(x: CGFloat(scale), y: CGFloat(scale))
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(cgContext: context, flipped: false)
        image.draw(in: NSRect(origin: .zero, size: image.size))
        NSGraphicsContext.restoreGraphicsState()
        context.restoreGState()
        // Keep the drawing's coverage and alphas; replace its black with the bar's ink.
        context.saveGState()
        context.setBlendMode(.sourceIn)
        context.setFillColor(CGColor(gray: dark ? 1 : 0, alpha: 1))
        context.fill(all)
        context.restoreGState()
        context.endTransparencyLayer()
        let rendered = try XCTUnwrap(context.makeImage())
        return try XCTUnwrap(NSBitmapImageRep(cgImage: rendered).representation(using: .png, properties: [:]))
    }
}
