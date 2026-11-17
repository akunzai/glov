import CryptoKit
import Foundation
import GlovCore

/// The newest build of the newest Ubuntu LTS arm64 server cloud image, from cloud-images.ubuntu.com: picks the release
/// from Ubuntu's `meta-release`, checks `SHA256SUMS.gpg` with Ubuntu's cloud image signing key (`glov-image`, which pins
/// that key), downloads the image (resuming an interrupted download) and refuses it unless its SHA256 matches `SHA256SUMS`.
///
/// Cache layout, keyed by the image's SHA256 so that a new build (a new `release/` directory) is a new entry:
/// `<cache>/<sha256>/ubuntu-<version>-server-cloudimg-arm64.img`, which appears only once verified. Each run reads the
/// current `SHA256SUMS`, so an older build is never handed out, and a new download removes older builds.
public struct UbuntuImage: MediaDownloads {
    /// All Ubuntu releases, interim and LTS. Not `meta-release-lts`: that file serves LTS-to-LTS upgrades, which Ubuntu
    /// enables only from a new LTS's first point release, months after its cloud images are published.
    public static let metaReleaseURL = URL(string: "https://changelogs.ubuntu.com/meta-release")!
    /// `<codename>/release/` holds the newest build of a release; dated `release-YYYYMMDD/` directories stay fixed.
    public static let imagesURL = URL(string: "https://cloud-images.ubuntu.com/releases/")!

    public struct Release: Equatable, Sendable {
        /// e.g. `resolute`.
        public var codename: String
        /// e.g. `26.04`.
        public var version: String
        public var imageName: String { "ubuntu-\(version)-server-cloudimg-arm64.img" }
        public var directoryURL: URL { UbuntuImage.imagesURL.appendingPathComponent(codename + "/release/", isDirectory: true) }
    }

    /// `$GLOV_CACHE/ubuntu`, else `~/Library/Caches/glov/ubuntu`.
    public static func defaultCache(environment: [String: String] = ProcessInfo.processInfo.environment) -> String {
        MediaCache.root(environment: environment) + "/ubuntu"
    }

    public var cache: String
    var run: (String, [String]) throws -> (status: Int32, output: String) = LinuxProvisioner.runTool
    var download: (URL, String) throws -> Void = UbuntuImage.curl
    var imageTool: () throws -> String = { try DiskImageTool.locate() }
    var freeSpace: (String) -> UInt64? = UbuntuImage.defaultFreeSpace
    var log: (String) -> Void = UbuntuImage.defaultLog

    public init(cache: String = UbuntuImage.defaultCache()) { self.cache = cache }

    /// The newest LTS in a `meta-release` file: stanzas of `Key: value` lines, LTS versions spelled like `26.04.1 LTS`.
    static func newestLTS(metaRelease text: String) throws -> Release {
        var best: (key: [Int], release: Release)?
        for stanza in text.components(separatedBy: "\n\n") {
            var fields: [String: String] = [:]
            for line in stanza.split(separator: "\n") {
                let kv = line.split(separator: ":", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
                if kv.count == 2 { fields[kv[0]] = kv[1] }
            }
            guard let dist = fields["Dist"], let version = fields["Version"], version.hasSuffix(" LTS"),
                dist.allSatisfy({ $0.isASCII && $0.isLowercase }), !dist.isEmpty
            else { continue }
            let numbers = version.dropLast(" LTS".count).split(separator: ".").compactMap { Int($0) }
            guard numbers.count >= 2 else { continue }
            let key = Array(numbers.prefix(2))
            if best.map({ $0.key.lexicographicallyPrecedes(key) }) ?? true {
                best = (key, Release(codename: dist, version: String(format: "%d.%02d", key[0], key[1])))
            }
        }
        guard let best else { throw MediaError("meta-release lists no Ubuntu LTS release") }
        return best.release
    }

    /// The verified image in the cache, downloading the newest build first when it is not there yet.
    public func image() throws -> String {
        let fm = FileManager.default
        try fm.createDirectory(atPath: cache, withIntermediateDirectories: true)
        let index = cache + "/index"
        try? fm.removeItem(atPath: index)
        try fm.createDirectory(atPath: index, withIntermediateDirectories: true)
        defer { try? fm.removeItem(atPath: index) }

        try download(Self.metaReleaseURL, index + "/meta-release")
        let release = try Self.newestLTS(metaRelease: String(contentsOfFile: index + "/meta-release", encoding: .utf8))
        log("newest Ubuntu LTS: \(release.version) (\(release.codename))")
        let sums = index + "/SHA256SUMS", signature = index + "/SHA256SUMS.gpg"
        try download(release.directoryURL.appendingPathComponent("SHA256SUMS"), sums)
        try download(release.directoryURL.appendingPathComponent("SHA256SUMS.gpg"), signature)
        try verifySignature(signature, of: sums)
        guard let sha256 = Self.checksums(try String(contentsOfFile: sums, encoding: .utf8))[release.imageName] else {
            throw MediaError("SHA256SUMS in \(release.directoryURL.absoluteString) lists no \(release.imageName)")
        }
        guard sha256.count == 64, sha256.allSatisfy(\.isHexDigit) else { throw MediaError("SHA256SUMS has no valid SHA256 for \(release.imageName)") }

        let dir = cache + "/" + sha256, path = dir + "/" + release.imageName
        if fm.fileExists(atPath: path) {
            log("cached: \(path)")
            return path
        }
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try requireSpace(1 << 30, for: "the Ubuntu image")
        try downloadVerified(release.directoryURL.appendingPathComponent(release.imageName), to: path, expected: sha256, SHA256.self, "SHA256")
        for old in (try? fm.contentsOfDirectory(atPath: cache)) ?? [] where old != sha256 && old.count == 64 && old.allSatisfy(\.isHexDigit) {
            log("removing older build \(old)")
            try? fm.removeItem(atPath: cache + "/" + old)
        }
        return path
    }

    /// Checks `signature` over `file` with `glov-image`, which requires a good signature by Ubuntu's cloud image key
    /// (pinned by fingerprint in `diskimage/pgpsig`).
    func verifySignature(_ signature: String, of file: String) throws {
        log("verifying SHA256SUMS.gpg")
        let r = try run(try imageTool(), ["verify-ubuntu", file, signature])
        guard r.status == 0 else {
            throw MediaError("bad signature on SHA256SUMS: \(r.output.trimmingCharacters(in: .whitespacesAndNewlines)); refusing the image")
        }
    }
}
