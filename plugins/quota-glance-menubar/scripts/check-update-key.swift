import CryptoKit
import Foundation

// Sparkle's current export format is a base64-encoded, 32-byte Ed25519 seed.
let seedText = try String(contentsOfFile: CommandLine.arguments[1]).trimmingCharacters(in: .whitespacesAndNewlines)
guard let seed = Data(base64Encoded: seedText), seed.count == 32 else {
    fatalError("Expected a base64-encoded 32-byte Sparkle signing seed")
}
let key = try Curve25519.Signing.PrivateKey(rawRepresentation: seed)
let data = try Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[2]))
let plist = try PropertyListSerialization.propertyList(from: data, format: nil) as! [String: Any]
guard plist["SUPublicEDKey"] as? String == key.publicKey.rawRepresentation.base64EncodedString() else {
    fatalError("Sparkle signing secret does not match the public key in the app")
}
print("Update signing key matches the app's public key.")
