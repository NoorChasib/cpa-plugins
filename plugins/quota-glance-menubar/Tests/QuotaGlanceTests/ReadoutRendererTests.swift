import AppKit
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
        for style in [ReadoutStyle.letteredPair, .splitPill] {
            let image = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style), marks: try marks())
            XCTAssertTrue(image.isTemplate, "\(style)")
            XCTAssertEqual(image.size.height, ReadoutRenderer.height)
            XCTAssertEqual(image.size.width, image.size.width.rounded(), "\(style)")
            XCTAssertGreaterThan(image.size.width, 40, "\(style)")
            XCTAssertFalse(try pixels(image).allSatisfy { $0 == 0 }, "\(style) draws something")
        }
    }

    func testTwoDigitsOneDigitAndDashKeepTheWidth() throws {
        for style in [ReadoutStyle.letteredPair, .splitPill] {
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
        for style in [ReadoutStyle.letteredPair, .splitPill] {
            let on = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style), marks: m).size.width
            let off = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, badges: false), marks: m).size.width
            let icon = ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, icon: true), marks: m).size.width
            let three = ReadoutRenderer.image(cells: cells([94, 59, 36], providers: ["claude", "claude", "codex"]), options: options(style), marks: m).size.width
            XCTAssertLessThan(off, on, "\(style)")
            XCTAssertGreaterThan(icon, on, "\(style)")
            XCTAssertGreaterThan(three, on, "\(style)")
        }
        // A provider without a logo gets no badge, so its slot is narrower.
        let unknown = ReadoutRenderer.image(cells: cells([94, 59], providers: ["mystery", "mystery"]), options: options(.letteredPair), marks: m).size.width
        let off = ReadoutRenderer.image(cells: cells([94, 59]), options: options(.letteredPair, badges: false), marks: m).size.width
        XCTAssertEqual(unknown, off - 1, "Slots are 6pt apart with badges on, 7pt without")
    }

    func testLightDarkAndIncreasedContrastDrawDifferently() throws {
        let m = try marks()
        for style in [ReadoutStyle.letteredPair, .splitPill] {
            let dark = try pixels(ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, dark: true), marks: m))
            let light = try pixels(ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, dark: false), marks: m))
            let contrast = try pixels(ReadoutRenderer.image(cells: cells([94, 59]), options: options(style, dark: true, contrast: true), marks: m))
            XCTAssertNotEqual(dark, light, "\(style)")
            XCTAssertNotEqual(dark, contrast, "\(style)")
        }
    }

    func testLowReadingsUseTheSameInk() throws {
        // One ink at every level: 3% and 94% differ only by their glyphs, so the
        // most opaque pixel is the same.
        let m = try marks()
        let high = try pixels(ReadoutRenderer.image(cells: cells([94, 94]), options: options(.splitPill), marks: m))
        let low = try pixels(ReadoutRenderer.image(cells: cells([3, 3]), options: options(.splitPill), marks: m))
        let alpha = { (data: Data) in stride(from: 3, to: data.count, by: 4).map { data[$0] }.max() }
        XCTAssertEqual(alpha(high), alpha(low))
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
}
