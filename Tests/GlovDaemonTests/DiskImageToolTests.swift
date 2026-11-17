import Foundation
import Testing

@testable import GlovDaemonAPI

@Suite struct DiskImageToolTests {
    @Test func installCopiesTheHelperAndReplacesAnOlderOne() throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        let source = root + "/build", target = root + "/home/bin"
        try FileManager.default.createDirectory(atPath: source, withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: source + "/glov-image", contents: Data("new".utf8), attributes: [.posixPermissions: 0o755])
        try FileManager.default.createDirectory(atPath: target, withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: target + "/glov-image", contents: Data("old".utf8), attributes: [.posixPermissions: 0o755])
        try DiskImageTool.install(from: source, to: target)
        #expect(FileManager.default.contents(atPath: target + "/glov-image") == Data("new".utf8))
        #expect(FileManager.default.isExecutableFile(atPath: target + "/glov-image"))
        #expect(try FileManager.default.contentsOfDirectory(atPath: target) == ["glov-image"])
    }

    @Test func installRefusesABuildWithoutTheHelper() throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        #expect(throws: ProvisionError.self) { try DiskImageTool.install(from: root, to: root + "/bin") }
        #expect(!FileManager.default.fileExists(atPath: root + "/bin"))
    }

    @Test func locateTriesBesideTheExecutableThenTheHome() throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        let build = root + "/build", home = GlovHome(path: root + "/home")
        for dir in [build, home.path + "/bin"] { try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true) }
        #expect(throws: ProvisionError.self) { try DiskImageTool.locate(home: home, executable: build + "/glov") }
        FileManager.default.createFile(atPath: home.path + "/bin/glov-image", contents: Data(), attributes: [.posixPermissions: 0o755])
        #expect(try DiskImageTool.locate(home: home, executable: build + "/glov") == home.path + "/bin/glov-image")
        FileManager.default.createFile(atPath: build + "/glov-image", contents: Data(), attributes: [.posixPermissions: 0o755])
        #expect(try DiskImageTool.locate(home: home, executable: build + "/glov") == build + "/glov-image")
    }
}
