import Foundation
#if canImport(FoundationXML)
import FoundationXML
#endif

/// A single-colour provider logo read from a bundled SVG file
/// (`Resources/Logos/<providerID>.svg`). Every painted shape is drawn in the
/// readout's one ink; colours in the file are ignored, `none` and zero
/// opacity are honoured. Supported: `path`, `circle`, `ellipse`, `rect`,
/// `line`, `polyline`, `polygon`, nested `g`/`svg`, `transform`, `fill`,
/// `stroke`, `stroke-width`, `stroke-linecap`, `stroke-linejoin`,
/// `fill-rule`, `opacity` and the same properties in `style`. `defs`,
/// `clipPath`, `mask`, `symbol` and gradients are skipped, so a logo must be
/// plain shapes (which brand "glyph" SVGs are).
public struct LogoMark: Equatable {
    public struct Point: Equatable {
        public var x: Double
        public var y: Double
        public init(_ x: Double, _ y: Double) { self.x = x; self.y = y }
    }

    public struct Rect: Equatable {
        public var minX, minY, maxX, maxY: Double
        public var width: Double { maxX - minX }
        public var height: Double { maxY - minY }
    }

    public enum Command: Equatable {
        case move(Point)
        case line(Point)
        case curve(Point, Point, Point)
        case close
    }

    public enum LineCap: String, Equatable { case butt, round, square }
    public enum LineJoin: String, Equatable { case miter, round, bevel }

    public enum Paint: Equatable {
        case fill(evenOdd: Bool)
        case stroke(width: Double, cap: LineCap, join: LineJoin)
    }

    public struct Shape: Equatable {
        public var commands: [Command]
        public var paint: Paint
    }

    public enum ParseError: Error, Equatable {
        case invalidXML
        case noViewBox
        case noShapes
        case badPathData(String)
    }

    /// In user units: x, y, width, height.
    public let viewBox: Rect
    public let shapes: [Shape]

    public init(viewBox: Rect, shapes: [Shape]) {
        self.viewBox = viewBox
        self.shapes = shapes
    }

    public init(svg: String) throws {
        let reader = SVGReader()
        let parser = XMLParser(data: Data(svg.utf8))
        parser.delegate = reader
        guard parser.parse() else { throw reader.error ?? ParseError.invalidXML }
        if let error = reader.error { throw error }
        guard let box = reader.viewBox, box.width > 0, box.height > 0 else { throw ParseError.noViewBox }
        guard !reader.shapes.isEmpty else { throw ParseError.noShapes }
        viewBox = box
        shapes = reader.shapes
    }

    public init(contentsOf url: URL) throws {
        try self.init(svg: try String(contentsOf: url, encoding: .utf8))
    }

    /// The painted area in user units, strokes included (miter joins counted
    /// as half the stroke width, which is close enough for centring).
    public var inkBounds: Rect? {
        var result: Rect?
        for shape in shapes {
            guard var box = Self.bounds(of: shape.commands) else { continue }
            if case let .stroke(width, _, _) = shape.paint {
                box = Rect(minX: box.minX - width / 2, minY: box.minY - width / 2, maxX: box.maxX + width / 2, maxY: box.maxY + width / 2)
            }
            result = result.map { Rect(minX: min($0.minX, box.minX), minY: min($0.minY, box.minY), maxX: max($0.maxX, box.maxX), maxY: max($0.maxY, box.maxY)) } ?? box
        }
        return result
    }

    /// The fraction of a square badge left empty at its right edge once the
    /// viewBox is fitted into it. Used to centre the Split pill's ink.
    public var rightInkInset: Double {
        guard let ink = inkBounds else { return 0 }
        let side = max(viewBox.width, viewBox.height)
        let offset = (side - viewBox.width) / 2
        return max(0, min(1, (side - (ink.maxX - viewBox.minX + offset)) / side))
    }

    static func bounds(of commands: [Command]) -> Rect? {
        var xs: [Double] = [], ys: [Double] = []
        var current = Point(0, 0)
        for command in commands {
            switch command {
            case let .move(p), let .line(p):
                xs.append(p.x); ys.append(p.y); current = p
            case let .curve(c1, c2, p):
                xs.append(contentsOf: cubicExtremes(current.x, c1.x, c2.x, p.x))
                ys.append(contentsOf: cubicExtremes(current.y, c1.y, c2.y, p.y))
                current = p
            case .close:
                break
            }
        }
        guard let minX = xs.min(), let maxX = xs.max(), let minY = ys.min(), let maxY = ys.max() else { return nil }
        return Rect(minX: minX, minY: minY, maxX: maxX, maxY: maxY)
    }

    /// The endpoints and any interior extrema of one cubic coordinate.
    private static func cubicExtremes(_ p0: Double, _ p1: Double, _ p2: Double, _ p3: Double) -> [Double] {
        var values = [p0, p3]
        let a = -p0 + 3 * p1 - 3 * p2 + p3, b = 2 * (p0 - 2 * p1 + p2), c = p1 - p0
        var roots: [Double] = []
        if abs(a) < 1e-12 {
            if abs(b) > 1e-12 { roots.append(-c / b) }
        } else {
            let d = b * b - 4 * a * c
            if d >= 0 {
                let s = d.squareRoot()
                roots.append((-b + s) / (2 * a))
                roots.append((-b - s) / (2 * a))
            }
        }
        for t in roots where t > 0 && t < 1 {
            let u = 1 - t
            values.append(u * u * u * p0 + 3 * u * u * t * p1 + 3 * u * t * t * p2 + t * t * t * p3)
        }
        return values
    }
}

// MARK: - Affine transforms

struct Affine: Equatable {
    var a = 1.0, b = 0.0, c = 0.0, d = 1.0, e = 0.0, f = 0.0

    static let identity = Affine()

    /// self applied after `other`: (self ∘ other)(p) = self(other(p)).
    func concatenating(_ other: Affine) -> Affine {
        Affine(a: a * other.a + c * other.b, b: b * other.a + d * other.b,
               c: a * other.c + c * other.d, d: b * other.c + d * other.d,
               e: a * other.e + c * other.f + e, f: b * other.e + d * other.f + f)
    }

    func apply(_ p: LogoMark.Point) -> LogoMark.Point {
        LogoMark.Point(a * p.x + c * p.y + e, b * p.x + d * p.y + f)
    }

    var scale: Double { abs(a * d - b * c).squareRoot() }

    /// Parses an SVG transform list, for example `translate(2 3) scale(.5)`.
    static func parse(_ text: String) -> Affine {
        var result = Affine.identity
        var rest = Substring(text)
        while let open = rest.firstIndex(of: "("), let close = rest[open...].firstIndex(of: ")") {
            let name = rest[..<open].trimmingCharacters(in: CharacterSet(charactersIn: " ,\t\n\r"))
            let args = SVGNumbers.list(String(rest[rest.index(after: open)..<close]))
            var t = Affine.identity
            switch name {
            case "matrix" where args.count == 6:
                t = Affine(a: args[0], b: args[1], c: args[2], d: args[3], e: args[4], f: args[5])
            case "translate" where !args.isEmpty:
                t = Affine(e: args[0], f: args.count > 1 ? args[1] : 0)
            case "scale" where !args.isEmpty:
                t = Affine(a: args[0], d: args.count > 1 ? args[1] : args[0])
            case "rotate" where !args.isEmpty:
                let r = args[0] * .pi / 180
                let rot = Affine(a: cos(r), b: sin(r), c: -sin(r), d: cos(r))
                if args.count == 3 {
                    t = Affine(e: args[1], f: args[2]).concatenating(rot).concatenating(Affine(e: -args[1], f: -args[2]))
                } else {
                    t = rot
                }
            case "skewX" where !args.isEmpty:
                t = Affine(c: tan(args[0] * .pi / 180))
            case "skewY" where !args.isEmpty:
                t = Affine(b: tan(args[0] * .pi / 180))
            default:
                break
            }
            result = result.concatenating(t)
            rest = rest[rest.index(after: close)...]
        }
        return result
    }
}

// MARK: - Numbers and path data

enum SVGNumbers {
    static func list(_ text: String) -> [Double] {
        var scanner = PathScanner(text)
        var values: [Double] = []
        while let value = scanner.number() { values.append(value) }
        return values
    }

    /// A length attribute such as `16`, `16px` or `1.5`; percentages are not supported.
    static func length(_ text: String?) -> Double? {
        guard var text = text?.trimmingCharacters(in: .whitespaces), !text.isEmpty, !text.hasSuffix("%") else { return nil }
        for unit in ["px", "pt"] where text.hasSuffix(unit) { text.removeLast(unit.count) }
        return Double(text)
    }
}

struct PathScanner {
    private let chars: [Character]
    private(set) var index = 0

    init(_ text: String) { chars = Array(text) }

    var atEnd: Bool { mutating get { skipSeparators(); return index >= chars.count } }

    mutating func skipSeparators() {
        while index < chars.count, chars[index] == " " || chars[index] == "," || chars[index] == "\n" || chars[index] == "\t" || chars[index] == "\r" {
            index += 1
        }
    }

    mutating func command() -> Character? {
        skipSeparators()
        guard index < chars.count, chars[index].isLetter, chars[index] != "e", chars[index] != "E" else { return nil }
        defer { index += 1 }
        return chars[index]
    }

    /// An arc flag: a single 0 or 1, which may run straight into the next number.
    mutating func flag() -> Bool? {
        skipSeparators()
        guard index < chars.count, chars[index] == "0" || chars[index] == "1" else { return nil }
        defer { index += 1 }
        return chars[index] == "1"
    }

    mutating func number() -> Double? {
        skipSeparators()
        let start = index
        var i = index
        if i < chars.count, chars[i] == "+" || chars[i] == "-" { i += 1 }
        var digits = false, dot = false
        while i < chars.count {
            if chars[i].isASCII && chars[i].isNumber { digits = true; i += 1 }
            else if chars[i] == ".", !dot { dot = true; i += 1 }
            else { break }
        }
        guard digits else { index = start; return nil }
        if i < chars.count, chars[i] == "e" || chars[i] == "E" {
            var j = i + 1
            if j < chars.count, chars[j] == "+" || chars[j] == "-" { j += 1 }
            var expDigits = false
            while j < chars.count, chars[j].isASCII && chars[j].isNumber { expDigits = true; j += 1 }
            if expDigits { i = j }
        }
        index = i
        return Double(String(chars[start..<i]))
    }
}

enum PathData {
    typealias Point = LogoMark.Point

    /// Absolute move/line/cubic/close commands for an SVG `d` attribute.
    static func parse(_ d: String) throws -> [LogoMark.Command] {
        var s = PathScanner(d)
        var out: [LogoMark.Command] = []
        var current = Point(0, 0), start = Point(0, 0)
        var lastCubic: Point?, lastQuad: Point?
        var command: Character?
        while !s.atEnd {
            if let next = s.command() {
                command = next
            } else if command == nil || command == "Z" || command == "z" {
                // Numbers with no command, or after a close, would never be consumed.
                throw LogoMark.ParseError.badPathData(d)
            }
            guard let cmd = command else { break }
            let relative = cmd.isLowercase
            let base = relative ? current : Point(0, 0)
            func point() throws -> Point {
                guard let x = s.number(), let y = s.number() else { throw LogoMark.ParseError.badPathData(d) }
                return Point(base.x + x, base.y + y)
            }
            func value() throws -> Double {
                guard let v = s.number() else { throw LogoMark.ParseError.badPathData(d) }
                return v
            }
            var cubic: Point?, quad: Point?
            switch cmd.uppercased() {
            case "M":
                current = try point()
                start = current
                out.append(.move(current))
                // Further pairs after a move are implicit line-tos.
                command = relative ? "l" : "L"
            case "L":
                current = try point()
                out.append(.line(current))
            case "H":
                current = Point(try value() + (relative ? current.x : 0), current.y)
                out.append(.line(current))
            case "V":
                current = Point(current.x, try value() + (relative ? current.y : 0))
                out.append(.line(current))
            case "C":
                let c1 = try point(), c2 = try point(), p = try point()
                out.append(.curve(c1, c2, p))
                cubic = c2
                current = p
            case "S":
                let c1 = lastCubic.map { Point(2 * current.x - $0.x, 2 * current.y - $0.y) } ?? current
                let c2 = try point(), p = try point()
                out.append(.curve(c1, c2, p))
                cubic = c2
                current = p
            case "Q":
                let q = try point(), p = try point()
                out.append(quadratic(current, q, p))
                quad = q
                current = p
            case "T":
                let q = lastQuad.map { Point(2 * current.x - $0.x, 2 * current.y - $0.y) } ?? current
                let p = try point()
                out.append(quadratic(current, q, p))
                quad = q
                current = p
            case "A":
                let rx = try value(), ry = try value(), angle = try value()
                guard let large = s.flag(), let sweep = s.flag() else { throw LogoMark.ParseError.badPathData(d) }
                let p = try point()
                out.append(contentsOf: arc(from: current, to: p, rx: rx, ry: ry, angle: angle, large: large, sweep: sweep))
                current = p
            case "Z":
                out.append(.close)
                current = start
            default:
                throw LogoMark.ParseError.badPathData(d)
            }
            lastCubic = cubic
            lastQuad = quad
        }
        return out
    }

    static func quadratic(_ p0: Point, _ q: Point, _ p: Point) -> LogoMark.Command {
        .curve(Point(p0.x + 2 / 3 * (q.x - p0.x), p0.y + 2 / 3 * (q.y - p0.y)),
               Point(p.x + 2 / 3 * (q.x - p.x), p.y + 2 / 3 * (q.y - p.y)), p)
    }

    /// SVG endpoint arc → cubic segments of at most 90°.
    static func arc(from p0: Point, to p: Point, rx rxIn: Double, ry ryIn: Double, angle: Double, large: Bool, sweep: Bool) -> [LogoMark.Command] {
        if p0 == p { return [] }
        var rx = abs(rxIn), ry = abs(ryIn)
        if rx == 0 || ry == 0 { return [.line(p)] }
        let phi = angle * .pi / 180, cosP = cos(phi), sinP = sin(phi)
        let dx = (p0.x - p.x) / 2, dy = (p0.y - p.y) / 2
        let x1 = cosP * dx + sinP * dy, y1 = -sinP * dx + cosP * dy
        let lambda = (x1 * x1) / (rx * rx) + (y1 * y1) / (ry * ry)
        if lambda > 1 { rx *= lambda.squareRoot(); ry *= lambda.squareRoot() }
        let num = rx * rx * ry * ry - rx * rx * y1 * y1 - ry * ry * x1 * x1
        let den = rx * rx * y1 * y1 + ry * ry * x1 * x1
        var coef = den == 0 ? 0 : (max(0, num) / den).squareRoot()
        if large == sweep { coef = -coef }
        let cx1 = coef * rx * y1 / ry, cy1 = -coef * ry * x1 / rx
        let cx = cosP * cx1 - sinP * cy1 + (p0.x + p.x) / 2
        let cy = sinP * cx1 + cosP * cy1 + (p0.y + p.y) / 2
        func angleBetween(_ ux: Double, _ uy: Double, _ vx: Double, _ vy: Double) -> Double {
            let dot = ux * vx + uy * vy, len = (ux * ux + uy * uy).squareRoot() * (vx * vx + vy * vy).squareRoot()
            var a = acos(max(-1, min(1, dot / len)))
            if ux * vy - uy * vx < 0 { a = -a }
            return a
        }
        let theta1 = angleBetween(1, 0, (x1 - cx1) / rx, (y1 - cy1) / ry)
        var delta = angleBetween((x1 - cx1) / rx, (y1 - cy1) / ry, (-x1 - cx1) / rx, (-y1 - cy1) / ry)
        if !sweep && delta > 0 { delta -= 2 * .pi }
        if sweep && delta < 0 { delta += 2 * .pi }
        let segments = max(1, Int((abs(delta) / (.pi / 2)).rounded(.up)))
        let step = delta / Double(segments)
        let k = 4.0 / 3.0 * tan(step / 4)
        func at(_ t: Double) -> (Point, Point) {
            let x = rx * cos(t), y = ry * sin(t), tx = -rx * sin(t), ty = ry * cos(t)
            return (Point(cosP * x - sinP * y + cx, sinP * x + cosP * y + cy),
                    Point(cosP * tx - sinP * ty, sinP * tx + cosP * ty))
        }
        var out: [LogoMark.Command] = []
        var t = theta1
        for i in 0..<segments {
            let (a, da) = at(t), (b, db) = at(t + step)
            let end = i == segments - 1 ? p : b
            out.append(.curve(Point(a.x + k * da.x, a.y + k * da.y), Point(b.x - k * db.x, b.y - k * db.y), end))
            t += step
        }
        return out
    }
}

// MARK: - XML

final class SVGReader: NSObject, XMLParserDelegate {
    struct Style {
        var fill = true
        var evenOdd = false
        var stroke = false
        var strokeWidth = 1.0
        var cap = LogoMark.LineCap.butt
        var join = LogoMark.LineJoin.miter
        var visible = true
        var transform = Affine.identity
    }

    var viewBox: LogoMark.Rect?
    var shapes: [LogoMark.Shape] = []
    var error: LogoMark.ParseError?
    private var stack: [Style] = [Style()]
    private var skipDepth = 0
    private static let skipped: Set<String> = ["defs", "clipPath", "mask", "symbol", "linearGradient", "radialGradient", "pattern", "marker", "title", "desc", "metadata", "style", "filter"]

    func parser(_ parser: XMLParser, didStartElement name: String, namespaceURI: String?, qualifiedName: String?, attributes: [String: String] = [:]) {
        let element = name.split(separator: ":").last.map(String.init) ?? name
        if skipDepth > 0 || Self.skipped.contains(element) {
            skipDepth += 1
            return
        }
        var attrs = attributes
        if let style = attributes["style"] {
            for declaration in style.split(separator: ";") {
                let pair = declaration.split(separator: ":", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
                if pair.count == 2 { attrs[pair[0]] = pair[1] }
            }
        }
        var style = stack.last ?? Style()
        apply(attrs, to: &style)
        stack.append(style)
        if element == "svg", viewBox == nil {
            let numbers = attrs["viewBox"].map(SVGNumbers.list) ?? []
            if numbers.count == 4 {
                viewBox = LogoMark.Rect(minX: numbers[0], minY: numbers[1], maxX: numbers[0] + numbers[2], maxY: numbers[1] + numbers[3])
            } else if let w = SVGNumbers.length(attrs["width"]), let h = SVGNumbers.length(attrs["height"]) {
                viewBox = LogoMark.Rect(minX: 0, minY: 0, maxX: w, maxY: h)
            }
            return
        }
        guard style.visible else { return }
        do {
            guard let commands = try geometry(element, attrs), !commands.isEmpty else { return }
            let placed = commands.map { transform($0, style.transform) }
            if style.fill { shapes.append(.init(commands: placed, paint: .fill(evenOdd: style.evenOdd))) }
            if style.stroke, style.strokeWidth > 0 {
                shapes.append(.init(commands: placed, paint: .stroke(width: style.strokeWidth * style.transform.scale, cap: style.cap, join: style.join)))
            }
        } catch let failure as LogoMark.ParseError {
            error = failure
            parser.abortParsing()
        } catch {
            self.error = .invalidXML
            parser.abortParsing()
        }
    }

    func parser(_ parser: XMLParser, didEndElement name: String, namespaceURI: String?, qualifiedName: String?) {
        if skipDepth > 0 { skipDepth -= 1; return }
        if stack.count > 1 { stack.removeLast() }
    }

    private func apply(_ attrs: [String: String], to style: inout Style) {
        func painted(_ value: String) -> Bool {
            let v = value.trimmingCharacters(in: .whitespaces).lowercased()
            return v != "none" && v != "transparent"
        }
        if let fill = attrs["fill"] { style.fill = painted(fill) }
        if let stroke = attrs["stroke"] { style.stroke = painted(stroke) }
        if let width = SVGNumbers.length(attrs["stroke-width"]) { style.strokeWidth = width }
        if let rule = attrs["fill-rule"] { style.evenOdd = rule.trimmingCharacters(in: .whitespaces) == "evenodd" }
        if let cap = attrs["stroke-linecap"].flatMap({ LogoMark.LineCap(rawValue: $0.trimmingCharacters(in: .whitespaces)) }) { style.cap = cap }
        if let join = attrs["stroke-linejoin"].flatMap({ LogoMark.LineJoin(rawValue: $0.trimmingCharacters(in: .whitespaces)) }) { style.join = join }
        for key in ["opacity", "fill-opacity"] where Double(attrs[key] ?? "") == 0 {
            if key == "opacity" { style.visible = false } else { style.fill = false }
        }
        if Double(attrs["stroke-opacity"] ?? "") == 0 { style.stroke = false }
        if attrs["display"] == "none" || attrs["visibility"] == "hidden" { style.visible = false }
        if let t = attrs["transform"] { style.transform = style.transform.concatenating(Affine.parse(t)) }
    }

    private func geometry(_ element: String, _ a: [String: String]) throws -> [LogoMark.Command]? {
        typealias P = LogoMark.Point
        func n(_ key: String) -> Double { SVGNumbers.length(a[key]) ?? 0 }
        switch element {
        case "path":
            return try a["d"].map(PathData.parse)
        case "circle":
            return ellipse(cx: n("cx"), cy: n("cy"), rx: n("r"), ry: n("r"))
        case "ellipse":
            return ellipse(cx: n("cx"), cy: n("cy"), rx: n("rx"), ry: n("ry"))
        case "line":
            return [.move(P(n("x1"), n("y1"))), .line(P(n("x2"), n("y2")))]
        case "polyline", "polygon":
            let v = SVGNumbers.list(a["points"] ?? "")
            guard v.count >= 4 else { return nil }
            var out: [LogoMark.Command] = [.move(P(v[0], v[1]))]
            for i in stride(from: 2, to: v.count - 1, by: 2) { out.append(.line(P(v[i], v[i + 1]))) }
            if element == "polygon" { out.append(.close) }
            return out
        case "rect":
            let x = n("x"), y = n("y"), w = n("width"), h = n("height")
            guard w > 0, h > 0 else { return nil }
            var rx = SVGNumbers.length(a["rx"]), ry = SVGNumbers.length(a["ry"])
            if rx == nil { rx = ry }
            if ry == nil { ry = rx }
            let r = (min(rx ?? 0, w / 2), min(ry ?? 0, h / 2))
            if r.0 <= 0 || r.1 <= 0 {
                return [.move(P(x, y)), .line(P(x + w, y)), .line(P(x + w, y + h)), .line(P(x, y + h)), .close]
            }
            var out: [LogoMark.Command] = [.move(P(x + r.0, y)), .line(P(x + w - r.0, y))]
            out += PathData.arc(from: P(x + w - r.0, y), to: P(x + w, y + r.1), rx: r.0, ry: r.1, angle: 0, large: false, sweep: true)
            out.append(.line(P(x + w, y + h - r.1)))
            out += PathData.arc(from: P(x + w, y + h - r.1), to: P(x + w - r.0, y + h), rx: r.0, ry: r.1, angle: 0, large: false, sweep: true)
            out.append(.line(P(x + r.0, y + h)))
            out += PathData.arc(from: P(x + r.0, y + h), to: P(x, y + h - r.1), rx: r.0, ry: r.1, angle: 0, large: false, sweep: true)
            out.append(.line(P(x, y + r.1)))
            out += PathData.arc(from: P(x, y + r.1), to: P(x + r.0, y), rx: r.0, ry: r.1, angle: 0, large: false, sweep: true)
            out.append(.close)
            return out
        default:
            return nil
        }
    }

    private func ellipse(cx: Double, cy: Double, rx: Double, ry: Double) -> [LogoMark.Command]? {
        guard rx > 0, ry > 0 else { return nil }
        typealias P = LogoMark.Point
        let k = 0.5522847498
        return [
            .move(P(cx + rx, cy)),
            .curve(P(cx + rx, cy + k * ry), P(cx + k * rx, cy + ry), P(cx, cy + ry)),
            .curve(P(cx - k * rx, cy + ry), P(cx - rx, cy + k * ry), P(cx - rx, cy)),
            .curve(P(cx - rx, cy - k * ry), P(cx - k * rx, cy - ry), P(cx, cy - ry)),
            .curve(P(cx + k * rx, cy - ry), P(cx + rx, cy - k * ry), P(cx + rx, cy)),
            .close,
        ]
    }

    private func transform(_ command: LogoMark.Command, _ t: Affine) -> LogoMark.Command {
        switch command {
        case let .move(p): return .move(t.apply(p))
        case let .line(p): return .line(t.apply(p))
        case let .curve(a, b, p): return .curve(t.apply(a), t.apply(b), t.apply(p))
        case .close: return .close
        }
    }
}
