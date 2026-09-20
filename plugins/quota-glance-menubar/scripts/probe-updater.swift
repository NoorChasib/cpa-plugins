// Exercise the actual Sparkle updater and the packaged app's public key without
// offering an installation or launching the dashboard.
import AppKit
import Sparkle

@MainActor
final class Probe: NSObject, SPUUpdaterDelegate {
    let feed: String
    let expectValid: Bool
    var validated = false
    init(feed: String, expectValid: Bool) {
        self.feed = feed
        self.expectValid = expectValid
    }
    func feedURLString(for updater: SPUUpdater) -> String? { feed }
    func updater(_ updater: SPUUpdater, didFinishLoading appcast: SUAppcast) {
        validated = appcast.signingValidationStatus == .succeeded
    }
    func updater(_ updater: SPUUpdater, didFinishUpdateCycleFor updateCheck: SPUUpdateCheck, error: Error?) {
        let noNewUpdate = (error as NSError?)?.code == 1001 // SUNoUpdateError
        if expectValid ? (validated && (error == nil || noNewUpdate)) : (!validated && error != nil && !noNewUpdate) {
            print(expectValid ? "Sparkle accepted the signed feed with the app's public key." : "Sparkle rejected the altered feed.")
            exit(0)
        }
        fputs("Unexpected Sparkle feed validation result: \(String(describing: error))\n", stderr)
        exit(1)
    }
}

_ = NSApplication.shared
let bundle = Bundle(path: CommandLine.arguments[1])!
let probe = Probe(feed: CommandLine.arguments[2], expectValid: CommandLine.arguments[3] == "valid")
let driver = SPUStandardUserDriver(hostBundle: bundle, delegate: nil)
let updater = SPUUpdater(hostBundle: bundle, applicationBundle: bundle, userDriver: driver, delegate: probe)
try updater.start()
updater.checkForUpdateInformation()
DispatchQueue.main.asyncAfter(deadline: .now() + 30) {
    fputs("Timed out probing the update feed.\n", stderr)
    exit(1)
}
RunLoop.main.run()
