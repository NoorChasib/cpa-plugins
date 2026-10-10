import Foundation
import XCTest
@testable import GlanceCore

final class LogoMarkTests: XCTestCase {
    private let logos = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
        .deletingLastPathComponent().appendingPathComponent("Resources/Logos")

    func testTheThreeBundledLogosParse() throws {
        for provider in ["claude", "codex", "xai"] {
            let mark = try LogoMark(contentsOf: logos.appendingPathComponent("\(provider).svg"))
            let box = mark.viewBox
            XCTAssertEqual(box.width, box.height, accuracy: box.width * 0.01, "\(provider): the viewBox must be square")
            XCTAssertTrue(mark.shapes.allSatisfy { if case .fill = $0.paint { return true } else { return false } }, provider)
            // The whole viewBox is fitted into the badge, so the ink must fill it
            // edge to edge or the logo draws smaller than the others.
            let ink = try XCTUnwrap(mark.inkBounds, provider)
            XCTAssertGreaterThan(max(ink.width, ink.height), box.width * 0.98, provider)
            XCTAssertGreaterThanOrEqual(ink.minX, box.minX - 0.01, provider)
            XCTAssertLessThanOrEqual(ink.maxX, box.maxX + 0.01, provider)
            XCTAssertGreaterThanOrEqual(mark.rightInkInset, 0, provider)
            XCTAssertLessThan(mark.rightInkInset, 0.02, provider)
        }
        let codex = try LogoMark(contentsOf: logos.appendingPathComponent("codex.svg"))
        XCTAssertEqual(codex.shapes.first?.paint, .fill(evenOdd: true), "The blossom's knot is cut out by the even-odd rule")
        let xai = try LogoMark(contentsOf: logos.appendingPathComponent("xai.svg"))
        XCTAssertEqual(xai.shapes.count, 2, "The Grok mark is two strokes drawn as fills")
    }

    func testPathDataCoversEveryCommand() throws {
        let commands = try PathData.parse("M1,1 h2 v2 H1 V1 l1-1 L4.5.5 c1 0 1 1 1 1 s0 1 1 1 q1 0 1 1 t1 1 a1 1 0 0 1 1 1 A1 1 0 10.5.5 Z m1 1 2 2z")
        XCTAssertEqual(commands.first, .move(.init(1, 1)))
        XCTAssertEqual(commands[1], .line(.init(3, 1)))
        XCTAssertEqual(commands[2], .line(.init(3, 3)))
        XCTAssertEqual(commands[6], .line(.init(4.5, 0.5)), "Numbers may run together: 4.5.5")
        XCTAssertTrue(commands.contains(.close))
        // The relative move after a close starts from the subpath's start (1,1), and its extra pair is a line-to.
        let tail = Array(commands.suffix(3))
        XCTAssertEqual(tail[0], .move(.init(2, 2)))
        XCTAssertEqual(tail[1], .line(.init(4, 4)))
        XCTAssertThrowsError(try PathData.parse("1 2"))
        XCTAssertThrowsError(try PathData.parse("M1 2 Z 3 4"))
        XCTAssertThrowsError(try PathData.parse("M1"))
    }

    func testArcsBecomeCubicsThatReachTheEndPoint() throws {
        let commands = try PathData.parse("M0 8 A8 8 0 0 1 16 8")
        guard case let .curve(_, _, end) = commands.last else { return XCTFail("expected curves") }
        XCTAssertEqual(end, .init(16, 8))
        let box = try XCTUnwrap(LogoMark.bounds(of: commands))
        XCTAssertEqual(box.minY, 0, accuracy: 0.01, "A half circle above the chord peaks at y = 0")
    }

    func testShapesGroupsTransformsAndStyles() throws {
        let svg = """
        <?xml version="1.0"?>
        <svg xmlns="http://www.w3.org/2000/svg" width="24px" height="24px">
          <title>Example</title>
          <defs><clipPath id="c"><rect width="24" height="24"/></clipPath></defs>
          <g fill="none" stroke="#000" stroke-width="2" transform="translate(2 2)">
            <rect x="0" y="0" width="10" height="10" rx="2"/>
            <circle cx="15" cy="15" r="3" style="fill:#123;stroke:none"/>
          </g>
          <polygon points="0,0 4,0 4,4" fill-rule="evenodd"/>
          <ellipse cx="20" cy="4" rx="2" ry="1" opacity="0"/>
          <line x1="0" y1="20" x2="4" y2="20" stroke="red" stroke-linecap="round"/>
        </svg>
        """
        let mark = try LogoMark(svg: svg)
        XCTAssertEqual(mark.viewBox, LogoMark.Rect(minX: 0, minY: 0, maxX: 24, maxY: 24))
        XCTAssertEqual(mark.shapes.map(\.paint), [
            .stroke(width: 2, cap: .butt, join: .miter),
            .fill(evenOdd: false),
            .fill(evenOdd: true),
            .fill(evenOdd: false), // a line has no area but is still a filled shape
            .stroke(width: 1, cap: .round, join: .miter),
        ])
        let rect = try XCTUnwrap(LogoMark.bounds(of: mark.shapes[0].commands))
        XCTAssertEqual(rect.minX, 2, accuracy: 1e-6)
        XCTAssertEqual(rect.maxX, 12, accuracy: 1e-6)
        let circle = try XCTUnwrap(LogoMark.bounds(of: mark.shapes[1].commands))
        XCTAssertEqual(circle.maxX, 20, accuracy: 1e-6)
    }

    func testScaledStrokesAndMatrices() throws {
        let mark = try LogoMark(svg: ##"<svg viewBox="0 0 10 10"><path d="M0 0L1 0" stroke="#000" fill="none" stroke-width="1" transform="scale(2) matrix(1 0 0 1 1 1)"/></svg>"##)
        XCTAssertEqual(mark.shapes.first?.commands.first, .move(.init(2, 2)))
        XCTAssertEqual(mark.shapes.first?.paint, .stroke(width: 2, cap: .butt, join: .miter))
    }

    func testRejectsFilesThatCannotBeDrawn() {
        XCTAssertThrowsError(try LogoMark(svg: "<svg viewBox=\"0 0 16 16\"></svg>")) { XCTAssertEqual($0 as? LogoMark.ParseError, .noShapes) }
        XCTAssertThrowsError(try LogoMark(svg: "<svg><path d=\"M0 0L1 1\"/></svg>")) { XCTAssertEqual($0 as? LogoMark.ParseError, .noViewBox) }
        XCTAssertThrowsError(try LogoMark(svg: "<svg viewBox=\"0 0 16 16\"><path d=\"M0 0 L\"/></svg>"))
        XCTAssertThrowsError(try LogoMark(svg: "not xml"))
    }
}
