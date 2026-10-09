import AppKit
import GlanceCore

/// Provider badges, read from the bundle's `Resources/Logos/<providerID>.svg`
/// (claude, codex and xai ship). Replacing those files and rebuilding swaps
/// the logos; see docs/logos.md. A provider without a readable file gets no
/// badge.
@MainActor
final class ProviderLogos {
    static let bundled = ProviderLogos(directory: Bundle.main.resourceURL?.appendingPathComponent("Logos", isDirectory: true))

    private let directory: URL?
    private var cache: [String: LogoMark?] = [:]

    init(directory: URL?) { self.directory = directory }

    func mark(for providerID: String) -> LogoMark? {
        if let cached = cache[providerID] { return cached }
        // Provider IDs come from the dashboard page: never let one name a path.
        let allowed = CharacterSet(charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-")
        var mark: LogoMark?
        if let directory, !providerID.isEmpty, providerID.count <= 64,
           providerID.unicodeScalars.allSatisfy(allowed.contains) {
            mark = try? LogoMark(contentsOf: directory.appendingPathComponent(providerID).appendingPathExtension("svg"))
        }
        cache[providerID] = mark
        return mark
    }

    /// A template image of the logo, for Settings rows and menus.
    func image(for providerID: String, size: CGFloat) -> NSImage? {
        guard let mark = mark(for: providerID) else { return nil }
        let image = NSImage(size: NSSize(width: size, height: size), flipped: false) { rect in
            guard let context = NSGraphicsContext.current?.cgContext else { return false }
            Self.draw(mark, in: rect, context: context)
            return true
        }
        image.isTemplate = true
        return image
    }

    /// Draws the mark fitted into `rect` (square), in opaque black, in a
    /// non-flipped context. Template rendering supplies the real ink.
    nonisolated static func draw(_ mark: LogoMark, in rect: CGRect, context: CGContext, alpha: CGFloat = 1) {
        let box = mark.viewBox
        let side = max(box.width, box.height)
        let scale = rect.width / CGFloat(side)
        let dx = (side - box.width) / 2, dy = (side - box.height) / 2
        func map(_ p: LogoMark.Point) -> CGPoint {
            CGPoint(x: rect.minX + CGFloat(p.x - box.minX + dx) * scale,
                    y: rect.maxY - CGFloat(p.y - box.minY + dy) * scale)
        }
        context.saveGState()
        defer { context.restoreGState() }
        let ink = CGColor(gray: 0, alpha: alpha)
        context.setFillColor(ink)
        context.setStrokeColor(ink)
        for shape in mark.shapes {
            let path = CGMutablePath()
            for command in shape.commands {
                switch command {
                case let .move(p): path.move(to: map(p))
                case let .line(p): path.addLine(to: map(p))
                case let .curve(a, b, p): path.addCurve(to: map(p), control1: map(a), control2: map(b))
                case .close: path.closeSubpath()
                }
            }
            context.addPath(path)
            switch shape.paint {
            case let .fill(evenOdd):
                context.fillPath(using: evenOdd ? .evenOdd : .winding)
            case let .stroke(width, cap, join):
                context.setLineWidth(CGFloat(width) * scale)
                context.setLineCap(cap == .round ? .round : cap == .square ? .square : .butt)
                context.setLineJoin(join == .round ? .round : join == .bevel ? .bevel : .miter)
                context.strokePath()
            }
        }
    }
}
