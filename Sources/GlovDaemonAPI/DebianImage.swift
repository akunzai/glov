import CryptoKit
import Foundation
import GlovCore

/// The current stable Debian genericcloud arm64 qcow2 image from cloud.debian.org, verified against `SHA512SUMS`
/// and cached under `$GLOV_CACHE/debian/<sha512>/`.
public struct DebianImage: MediaDownloads {
    public static let cloudURL = URL(string: "https://cloud.debian.org/images/cloud/")!
    public static let defaultCodename = "trixie"
    public static let defaultVersion = "13"

    public struct Release: Equatable, Sendable {
        public var codename: String
        public var version: String

        public init(codename: String, version: String) {
            self.codename = codename
            self.version = version
        }
        public var imageName: String { "debian-\(version)-genericcloud-arm64.qcow2" }
        public var directoryURL: URL { DebianImage.cloudURL.appendingPathComponent(codename + "/latest/", isDirectory: true) }
    }

    /// `$GLOV_CACHE/debian`, else `~/Library/Caches/glov/debian`.
    public static func defaultCache(environment: [String: String] = ProcessInfo.processInfo.environment) -> String {
        MediaCache.root(environment: environment) + "/debian"
    }

    public var cache: String
    public var release: Release
    var download: (URL, String) throws -> Void = DebianImage.curl
    var freeSpace: (String) -> UInt64? = DebianImage.defaultFreeSpace
    var log: (String) -> Void = DebianImage.defaultLog

    public init(cache: String = DebianImage.defaultCache(), release: Release = .init(codename: DebianImage.defaultCodename, version: DebianImage.defaultVersion)) {
        self.cache = cache
        self.release = release
    }

    public func image() throws -> String {
        let fm = FileManager.default
        try fm.createDirectory(atPath: cache, withIntermediateDirectories: true)
        let index = cache + "/index"
        try? fm.removeItem(atPath: index)
        try fm.createDirectory(atPath: index, withIntermediateDirectories: true)
        defer { try? fm.removeItem(atPath: index) }

        log("Debian cloud image: \(release.version) (\(release.codename))")
        let sums = index + "/SHA512SUMS"
        try download(release.directoryURL.appendingPathComponent("SHA512SUMS"), sums)
        guard let sha512 = Self.checksums(try String(contentsOfFile: sums, encoding: .utf8))[release.imageName] else {
            throw MediaError("SHA512SUMS in \(release.directoryURL.absoluteString) lists no \(release.imageName)")
        }
        guard sha512.count == 128, sha512.allSatisfy(\.isHexDigit) else {
            throw MediaError("SHA512SUMS has no valid SHA512 for \(release.imageName)")
        }

        let dir = cache + "/" + sha512, path = dir + "/" + release.imageName
        if fm.fileExists(atPath: path) {
            log("cached: \(path)")
            return path
        }
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try requireSpace(1 << 30, for: "the Debian image")
        try downloadVerified(release.directoryURL.appendingPathComponent(release.imageName), to: path, expected: sha512, SHA512.self, "SHA512")
        for old in (try? fm.contentsOfDirectory(atPath: cache)) ?? [] where old != sha512 && old.count == 128 && old.allSatisfy(\.isHexDigit) {
            log("removing older build \(old)")
            try? fm.removeItem(atPath: cache + "/" + old)
        }
        return path
    }
}
