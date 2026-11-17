import CryptoKit
import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

@Suite struct UbuntuReleaseTests {
    /// Trimmed from https://changelogs.ubuntu.com/meta-release.
    static let metaRelease = """
        Dist: noble
        Name: Noble Numbat
        Version: 24.04.5 LTS
        Date: Thu, 25 April 2024 00:24:04 UTC
        Supported: 1

        Dist: questing
        Name: Questing Quokka
        Version: 25.10
        Supported: 0

        Dist: resolute
        Name: Resolute Raccoon
        Version: 26.04.1 LTS
        Date: Thu, 23 April 2026 00:26:04 UTC
        Supported: 1
        Release-File: http://archive.ubuntu.com/ubuntu/dists/resolute-updates/Release

        """

    @Test func picksTheNewestLTSAndSkipsInterimReleases() throws {
        let release = try UbuntuImage.newestLTS(metaRelease: Self.metaRelease + "\nDist: stellar\nVersion: 26.10\nSupported: 1\n")
        #expect(release == .init(codename: "resolute", version: "26.04"))
        #expect(release.imageName == "ubuntu-26.04-server-cloudimg-arm64.img")
        #expect(release.directoryURL.absoluteString == "https://cloud-images.ubuntu.com/releases/resolute/release/")
    }

    @Test func movesToTheNextLTSOnceItIsListed() throws {
        let next = Self.metaRelease + "\nDist: tidy\nVersion: 28.04 LTS\nSupported: 1\n"
        #expect(try UbuntuImage.newestLTS(metaRelease: next) == .init(codename: "tidy", version: "28.04"))
    }

    @Test func aFileWithoutLTSReleasesIsRejected() {
        #expect(throws: MediaError("meta-release lists no Ubuntu LTS release")) {
            try UbuntuImage.newestLTS(metaRelease: "<html>not a meta-release</html>\n\nDist: ../x\nVersion: 26.04 LTS\n")
        }
    }

    @Test func checksumsMapEachFileToItsSHA256() {
        let sums = UbuntuImage.checksums(ubuntuSHA256SUMSFixture)
        #expect(sums["ubuntu-26.04-server-cloudimg-arm64.img"] == "8eb876c2ccc4b6ff89e4a65d511815923b285e04503ba465809bf587f763da24")
        #expect(sums["ubuntu-26.04-server-cloudimg-amd64.img"]?.count == 64)
        #expect(sums.count == 74)
    }

    @Test func theCacheFollowsGlovCache() {
        #expect(UbuntuImage.defaultCache(environment: ["GLOV_CACHE": "/Volumes/X/cache"]) == "/Volumes/X/cache/ubuntu")
        #expect(UbuntuImage.defaultCache(environment: [:]).hasSuffix("/Library/Caches/glov/ubuntu"))
    }
}

/// The real `glov-image`, built from `diskimage/` with Go, against Ubuntu's published SHA256SUMS and signature.
@Suite(.enabled(if: (try? VirtualMachine.find("go")) != nil, "needs Go to build glov-image"))
struct UbuntuSignatureTests {
    static let helper: Result<String, any Error> = Result {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let out = try shortTempDir() + "/glov-image"
        let r = try LinuxProvisioner.runTool(try VirtualMachine.find("go"), ["-C", repo.path + "/diskimage", "build", "-o", out, "./cmd/glov-image"])
        guard r.status == 0 else { throw MediaError("go build glov-image: \(r.output)") }
        return out
    }

    let dir: String
    init() throws { dir = try shortTempDir() }

    func verify(_ sums: String) async throws {
        try Data(sums.utf8).write(to: URL(fileURLWithPath: dir + "/SHA256SUMS"))
        try Data(ubuntuSHA256SUMSSignatureFixture.utf8).write(to: URL(fileURLWithPath: dir + "/SHA256SUMS.gpg"))
        let helper = try Self.helper.get()
        var m = UbuntuImage(cache: dir)
        m.log = { _ in }
        m.imageTool = { helper }
        let image = m, dir = dir
        try await offPool { try image.verifySignature(dir + "/SHA256SUMS.gpg", of: dir + "/SHA256SUMS") }
    }

    @Test func ubuntusSignatureIsAccepted() async throws {
        defer { try? FileManager.default.removeItem(atPath: dir) }
        try await verify(ubuntuSHA256SUMSFixture)
    }

    @Test func aChangedChecksumFileFailsTheSignature() async throws {
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let tampered = ubuntuSHA256SUMSFixture.replacingOccurrences(of: "8eb876c2ccc4b6ff", with: "0000000000000000")
        let error = await #expect(throws: MediaError.self) { try await verify(tampered) }
        #expect(error?.description.hasPrefix("bad signature on SHA256SUMS: glov-image: ") == true)
        #expect(error?.description.hasSuffix("; refusing the image") == true)
    }
}

@Suite struct UbuntuImageTests {
    static let imageBytes = Data("not really a cloud image".utf8)
    static let imageName = "ubuntu-26.04-server-cloudimg-arm64.img"
    static let base = "https://cloud-images.ubuntu.com/releases/resolute/release/"

    static func sha256(_ data: Data) -> String { SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined() }
    static func sums(for image: Data) -> String {
        "\(String(repeating: "1", count: 64)) *ubuntu-26.04-server-cloudimg-amd64.img\n\(sha256(image)) *\(imageName)\n"
    }

    let cache: String
    let calls = ToolCalls()
    init() throws { cache = try shortTempDir() }

    /// Fakes the network and gnupg: downloads append the file's bytes (`image` for the image, a SHA256SUMS listing
    /// `listed`), glov-image reports a good signature unless `badSignature`.
    func ubuntu(image: Data = imageBytes, listed: Data = imageBytes, badSignature: Bool = false, failImage: Bool = false) -> UbuntuImage {
        var m = UbuntuImage(cache: cache)
        let calls = calls
        m.imageTool = { "/fake/glov-image" }
        m.log = { _ in }
        m.download = { url, path in
            calls.add("download", [url.absoluteString, FileManager.default.fileExists(atPath: path) ? "resume" : "new"])
            let bytes: Data
            switch url.lastPathComponent {
            case "meta-release": bytes = Data(UbuntuReleaseTests.metaRelease.utf8)
            case "SHA256SUMS": bytes = Data(Self.sums(for: listed).utf8)
            case "SHA256SUMS.gpg": bytes = Data("signature".utf8)
            default:
                if failImage { throw MediaError("connection reset") }
                bytes = image
            }
            if !FileManager.default.fileExists(atPath: path) { FileManager.default.createFile(atPath: path, contents: nil) }
            let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
            try h.write(contentsOf: bytes.dropFirst(Int(try h.seekToEnd())))
            try h.close()
        }
        m.run = { exe, args in
            calls.add(URL(fileURLWithPath: exe).lastPathComponent, args)
            if exe.hasSuffix("/glov-image"), args.first == "verify-ubuntu", badSignature {
                return (1, "glov-image: \(args[2]): openpgp: invalid signature: RSA verification failure\n")
            }
            return (0, "")
        }
        return m
    }

    var downloads: [String] { calls.list.filter { $0.0 == "download" }.map { $0.1[0] } }
    func builds() throws -> [String] { try FileManager.default.contentsOfDirectory(atPath: cache).filter { $0.count == 64 }.sorted() }

    @Test func downloadsTheNewestLTSVerifiesItAndCachesItBySHA256() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let path = try ubuntu().image()
        let sha = Self.sha256(Self.imageBytes)
        #expect(path == cache + "/" + sha + "/" + Self.imageName)
        #expect(FileManager.default.contents(atPath: path) == Self.imageBytes)
        #expect(downloads == [UbuntuImage.metaReleaseURL.absoluteString, Self.base + "SHA256SUMS", Self.base + "SHA256SUMS.gpg", Self.base + Self.imageName])
        let verify = try #require(calls.list.first { $0.0 == "glov-image" })
        #expect(verify.1 == ["verify-ubuntu", cache + "/index/SHA256SUMS", cache + "/index/SHA256SUMS.gpg"])
        #expect(try FileManager.default.contentsOfDirectory(atPath: cache).sorted() == [sha])
        #expect(try FileManager.default.contentsOfDirectory(atPath: cache + "/" + sha) == [Self.imageName])
    }

    @Test func aBadSignatureIsRefusedBeforeTheImageIsDownloaded() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        #expect(throws: MediaError.self) { try ubuntu(badSignature: true).image() }
        #expect(!downloads.contains(Self.base + Self.imageName))
        #expect(try builds().isEmpty)
    }

    @Test func aSHA256MismatchIsRefusedAndThePartialDownloadDeleted() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let sha = Self.sha256(Self.imageBytes)
        let wrong = Data("not really a cloud imagf".utf8)
        #expect(throws: MediaError("SHA256 mismatch for \(Self.imageName): expected \(sha), got \(Self.sha256(wrong)); deleted the download")) {
            try ubuntu(image: wrong).image()
        }
        #expect(try FileManager.default.contentsOfDirectory(atPath: cache + "/" + sha).isEmpty)
    }

    @Test func aSecondRunUsesTheCachedBuildWithoutDownloadingTheImage() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let first = try ubuntu().image()
        #expect(try ubuntu().image() == first)
        #expect(downloads.filter { $0.hasSuffix(".img") }.count == 1)
        #expect(downloads.filter { $0.hasSuffix("/SHA256SUMS") }.count == 2)  // still checks for a newer build
    }

    @Test func aNewBuildIsDownloadedAndTheOlderOneIsNotReused() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let old = try ubuntu().image()
        let newBytes = Data("a newer cloud image".utf8)
        let new = try ubuntu(image: newBytes, listed: newBytes).image()
        #expect(new != old)
        #expect(new == cache + "/" + Self.sha256(newBytes) + "/" + Self.imageName)
        #expect(FileManager.default.contents(atPath: new) == newBytes)
        #expect(downloads.filter { $0.hasSuffix(".img") }.count == 2)
        #expect(try builds() == [Self.sha256(newBytes)])
    }

    @Test func anInterruptedDownloadResumes() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        #expect(throws: MediaError("connection reset")) { try ubuntu(failImage: true).image() }
        let partial = cache + "/" + Self.sha256(Self.imageBytes) + "/" + Self.imageName + ".partial"
        FileManager.default.createFile(atPath: partial, contents: Self.imageBytes.prefix(7))  // what the dropped connection left
        _ = try ubuntu().image()
        #expect(calls.list.filter { $0.0 == "download" && $0.1[0].hasSuffix(".img") }.map { $0.1[1] } == ["new", "resume"])
        #expect(!FileManager.default.fileExists(atPath: partial))
    }

    @Test func aChecksumFileWithoutTheImageIsRefused() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        var m = ubuntu()
        let download = m.download
        m.download = { url, path in
            if url.lastPathComponent == "SHA256SUMS" { FileManager.default.createFile(atPath: path, contents: Data("\(String(repeating: "1", count: 64)) *other.img\n".utf8)); return }
            try download(url, path)
        }
        #expect(throws: MediaError("SHA256SUMS in \(Self.base) lists no \(Self.imageName)")) { try m.image() }
    }

    @Test func tooLittleFreeSpaceIsRefusedBeforeDownloadingTheImage() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        var m = ubuntu()
        m.freeSpace = { _ in 10 }
        #expect(throws: MediaError.self) { try m.image() }
        #expect(!downloads.contains(Self.base + Self.imageName))
    }
}
