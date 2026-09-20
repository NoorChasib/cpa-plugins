import AppKit

// A native vector drawing, using the dashboard's background and quota blue.
// Render the standard iconset sizes without external fonts or image tooling.
let output = URL(fileURLWithPath: CommandLine.arguments[1], isDirectory: true)
try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
for size in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let pixels = size * scale
        let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
                                      bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true,
                                      isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
        let transform = NSAffineTransform()
        transform.scale(by: CGFloat(pixels) / 128)
        transform.concat()
        NSColor(calibratedRed: 24 / 255, green: 26 / 255, blue: 30 / 255, alpha: 1).setFill()
        NSBezierPath(roundedRect: NSRect(x: 8, y: 8, width: 112, height: 112), xRadius: 25, yRadius: 25).fill()
        for (index, height) in [36.0, 66.0, 48.0].enumerated() {
            let x = 29 + CGFloat(index) * 26
            NSColor(calibratedRed: 35 / 255, green: 38 / 255, blue: 44 / 255, alpha: 1).setFill()
            NSBezierPath(roundedRect: NSRect(x: x, y: 29, width: 18, height: 70), xRadius: 5, yRadius: 5).fill()
            NSColor(calibratedRed: 57 / 255, green: 135 / 255, blue: 229 / 255, alpha: 1).setFill()
            NSBezierPath(roundedRect: NSRect(x: x, y: 29, width: 18, height: height), xRadius: 5, yRadius: 5).fill()
        }
        NSGraphicsContext.restoreGraphicsState()
        let suffix = scale == 2 ? "@2x" : ""
        let file = output.appendingPathComponent("icon_\(size)x\(size)\(suffix).png")
        try bitmap.representation(using: .png, properties: [:])!.write(to: file)
    }
}
