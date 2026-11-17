import CryptoKit
import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

@Suite struct DebianImageTests {
    @Test func distrosAreUbuntuAndDebian() {
        #expect(LinuxDistro.allCases.map(\.rawValue) == ["ubuntu", "debian"])
    }

    static let imageBytes = Data("not really a debian qcow2".utf8)
    static let debianName = "debian-13-genericcloud-arm64.qcow2"

    static func sha512(_ data: Data) -> String {
        SHA512.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    let cache: String
    let calls = ToolCalls()
    init() throws { cache = try shortTempDir() }

    func debian(image: Data = imageBytes, listed: Data = imageBytes) -> DebianImage {
        var m = DebianImage(cache: cache)
        let calls = calls
        m.log = { _ in }
        m.download = { url, path in
            calls.add("download", [url.absoluteString])
            let bytes: Data
            if url.lastPathComponent == "SHA512SUMS" {
                bytes = Data("\(Self.sha512(listed))  \(Self.debianName)\n".utf8)
            } else {
                bytes = image
            }
            FileManager.default.createFile(atPath: path, contents: bytes)
        }
        return m
    }

    @Test func debianDownloadsVerifiesSHA512SUMSAndCachesBySHA512() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let path = try debian().image()
        let sha = Self.sha512(Self.imageBytes)
        #expect(path == cache + "/" + sha + "/" + Self.debianName)
        #expect(FileManager.default.contents(atPath: path) == Self.imageBytes)
        #expect(try debian().image() == path)
        #expect(calls.list.filter { $0.0 == "download" && $0.1[0].hasSuffix(".qcow2") }.count == 1)
        #expect(throws: MediaError.self) { try debian(image: Data("tampered".utf8), listed: Data("expected-other".utf8)).image() }
    }
}
