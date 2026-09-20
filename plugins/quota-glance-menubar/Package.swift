// swift-tools-version: 5.9
import PackageDescription

var dependencies: [Package.Dependency] = []
var products: [Product] = [.library(name: "GlanceCore", targets: ["GlanceCore"])]
var targets: [Target] = [
    .target(name: "GlanceCore"),
    .testTarget(name: "GlanceCoreTests", dependencies: ["GlanceCore"]),
]

// URL and navigation policy also run under Linux. The app requires Apple's SDK.
#if os(macOS)
dependencies.append(.package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.10.0"))
products.append(.executable(name: "QuotaGlance", targets: ["QuotaGlance"]))
targets.append(.executableTarget(
    name: "QuotaGlance",
    dependencies: ["GlanceCore", .product(name: "Sparkle", package: "Sparkle")],
    linkerSettings: [.unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"])]
))
targets.append(.testTarget(name: "QuotaGlanceTests", dependencies: ["QuotaGlance", "GlanceCore"]))
#endif

let package = Package(
    name: "QuotaGlanceMenuBar",
    platforms: [.macOS(.v13)],
    products: products,
    dependencies: dependencies,
    targets: targets
)
