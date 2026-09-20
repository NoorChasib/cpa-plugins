// swift-tools-version: 5.9
import PackageDescription

var products: [Product] = [.library(name: "GlanceCore", targets: ["GlanceCore"])]
var targets: [Target] = [
    .target(name: "GlanceCore"),
    .testTarget(name: "GlanceCoreTests", dependencies: ["GlanceCore"]),
]

// URL and navigation policy also run under Linux. The app requires Apple's SDK.
#if os(macOS)
products.append(.executable(name: "QuotaGlance", targets: ["QuotaGlance"]))
targets.append(.executableTarget(name: "QuotaGlance", dependencies: ["GlanceCore"]))
#endif

let package = Package(
    name: "QuotaGlanceMenuBar",
    platforms: [.macOS(.v13)],
    products: products,
    targets: targets
)
