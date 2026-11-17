import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemonAPI

/// Explicit permissions, so the result does not depend on the process-wide umask.
private func mkdirs(_ path: String) throws {
    try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o755])
}

private func write(_ path: String, _ data: Data) throws {
    try mkdirs((path as NSString).deletingLastPathComponent)
    try data.write(to: URL(fileURLWithPath: path))
}

private func pattern(_ count: Int, seed: UInt8) -> Data { Data((0..<count).map { UInt8(truncatingIfNeeded: $0 &* 31 &+ Int(seed)) }) }

@Suite struct FileTests {
    @Test func textRoundTripsThroughTheGuest() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let w = try await f.client.call(Method.fileWrite, Params.FileWrite(vm: "win11", path: "C:\\t\\a.txt", text: "héllo\n"), as: Results.FileWrite.self)
        #expect(w.size == 7)
        let write = try #require(f.fake.requests("file.write").first)
        #expect(write["truncate"] == true && write["mkdirs"] == true && write["offset"] == 0)
        let r = try await f.client.call(Method.fileRead, Params.FileRead(vm: "win11", path: "C:\\t\\a.txt"), as: Results.FileRead.self)
        #expect(r == .init(text: "héllo\n", size: 7))
    }

    @Test func aLargeWriteIsChunkedAt512KiB() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let text = String(repeating: "0123456789abcdef", count: 80_000)  // 1,280,000 bytes
        _ = try await f.client.call(Method.fileWrite, Params.FileWrite(vm: "win11", path: "C:\\big.txt", text: text), as: Results.FileWrite.self)
        let writes = f.fake.requests("file.write")
        #expect(writes.map { $0["offset"]?.intValue } == [0, 524_288, 1_048_576])
        #expect(writes.map { $0["truncate"] == true } == [true, false, false])
        #expect(f.fake.files["C:\\big.txt"] == Data(text.utf8))
    }

    @Test func fileReadRefusesWhatItCannotShowAndPointsAtCopyFromVM() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.files["C:\\big.bin"] = Data(repeating: 65, count: 256 * 1024 + 1)
        f.fake.files["C:\\ok.txt"] = Data(repeating: 65, count: 256 * 1024)
        f.fake.files["C:\\bin.dat"] = Data([0xff, 0xfe, 0x00])
        f.fake.dirs = ["C:\\d"]
        func fail(_ path: String) async -> String? { await f.failure(Method.fileRead, ["vm": "win11", "path": .string(path)])?.message }
        #expect(await fail("C:\\big.bin")?.contains("copy_from_vm") == true)
        #expect(await fail("C:\\bin.dat")?.contains("UTF-8") == true)
        #expect(await fail("C:\\d")?.contains("directory") == true)
        #expect(await fail("C:\\missing")?.contains("no such file") == true)
        let ok = try await f.client.call(Method.fileRead, Params.FileRead(vm: "win11", path: "C:\\ok.txt"), as: Results.FileRead.self)
        #expect(ok.size == 256 * 1024)
    }

    @Test func copyToVMSendsAWholeTreeInChunks() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let src = f.shared + "/proj"
        let big = Data(repeating: 0xff, count: 1_300_000)  // Slash-heavy base64 must fit each request frame.
        try write(src + "/a/b.txt", Data("b".utf8))
        try write(src + "/c.txt", Data("c".utf8))
        try write(src + "/empty.txt", Data())
        try write(src + "/bin/big.dll", big)
        try FileManager.default.createSymbolicLink(atPath: src + "/link", withDestinationPath: "/etc")
        let r = try await f.client.call(Method.copyToVM, Params.CopyToVM(vm: "win11", hostPath: src, guestPath: "C:\\work\\proj"), as: Results.Copy.self)
        #expect(r.files == 4 && r.bytes == 1_300_000 + 2)
        #expect(r.skipped.count == 1 && r.skipped[0].hasSuffix("/link"))
        #expect(f.fake.files["C:\\work\\proj\\a\\b.txt"] == Data("b".utf8))
        #expect(f.fake.files["C:\\work\\proj\\c.txt"] == Data("c".utf8))
        #expect(f.fake.files["C:\\work\\proj\\empty.txt"] == Data())
        #expect(f.fake.files["C:\\work\\proj\\bin\\big.dll"] == big)
        #expect(f.fake.files.count == 4)
    }

    @Test func copyToVMOfASingleFileUsesTheGuestPathAsTheFileName() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        try write(f.shared + "/one.txt", Data("1".utf8))
        _ = try await f.client.call(Method.copyToVM, Params.CopyToVM(vm: "win11", hostPath: f.shared + "/one.txt", guestPath: "C:\\x\\two.txt"), as: Results.Copy.self)
        #expect(f.fake.files["C:\\x\\two.txt"] == Data("1".utf8))
    }

    @Test(arguments: ["fetched", "fetched-é-檔案"]) func copyFromVMFetchesATreeAndRefusesNamesThatEscape(directory: String) async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let big = pattern(1_100_000, seed: 9)
        f.fake.dirs = ["C:\\out", "C:\\out\\sub"]
        f.fake.files = ["C:\\out\\x.txt": Data("x".utf8), "C:\\out\\sub\\y.bin": big, "C:\\out\\sub\\z.txt": Data()]
        f.fake.hostileEntries = [
            ["name": "..\\..\\evil.txt", "isDir": false, "size": 1], ["name": "../evil2.txt", "isDir": false, "size": 1],
            ["name": "..", "isDir": true, "size": 0],
        ]
        let dest = f.shared + "/" + directory
        let r = try await f.client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\out", hostPath: dest), as: Results.Copy.self)
        #expect(r.files == 3 && r.bytes == 1_100_001)
        #expect(try Data(contentsOf: URL(fileURLWithPath: dest + "/x.txt")) == Data("x".utf8))
        #expect(try Data(contentsOf: URL(fileURLWithPath: dest + "/sub/y.bin")) == big)
        #expect(FileManager.default.fileExists(atPath: dest + "/sub/z.txt"))
        #expect(r.skipped.count == 6)  // three hostile names in each of the two directories
        #expect(!FileManager.default.fileExists(atPath: f.shared + "/evil.txt") && !FileManager.default.fileExists(atPath: f.home.path + "/evil.txt"))
        // The first read of a missing guest path says so.
        let missing = await f.failure(Method.copyFromVM, ["vm": "win11", "guestPath": "C:\\nope", "hostPath": .string(dest + "/n")])
        #expect(missing?.message.contains("no such path in the guest") == true)
    }

    @Test(.timeLimit(.minutes(1))) func copyFromVMRetriesATimedOutChunk() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let data = Data(repeating: 0x42, count: 600_000)
        f.fake.files["C:\\retry.bin"] = data
        f.fake.droppedFileReads = 1
        f.fake.droppedFileReadOffset = 524_288
        let dest = f.shared + "/retry.bin"
        let result = try await f.client.call(Method.copyFromVM,
            Params.CopyFromVM(vm: "win11", guestPath: "C:\\retry.bin", hostPath: dest), as: Results.Copy.self)
        #expect(result.files == 1 && result.bytes == data.count)
        #expect(try Data(contentsOf: URL(fileURLWithPath: dest)) == data)
    }

    @Test(.timeLimit(.minutes(2))) func copyFromVMStopsAfterTwoReadTimeouts() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.files["C:\\timeout.bin"] = Data([0x42])
        f.fake.droppedFileReads = 2
        let dest = f.shared + "/timeout.bin"
        let error = try #require(await f.failure(Method.copyFromVM,
            ["vm": "win11", "guestPath": "C:\\timeout.bin", "hostPath": .string(dest)]))
        #expect(error.message.contains("C:\\timeout.bin") && error.message.contains("offset 0"))
        #expect(error.message.contains("file.read in time"))
        #expect(f.fake.requests("file.read").count == 2)
        #expect(!FileManager.default.fileExists(atPath: dest))
        #expect(try FileManager.default.contentsOfDirectory(atPath: f.shared).isEmpty)
    }

    @Test(arguments: [true, false])
    func failedDownloadPreservesTheDestinationAndReportsTheOffset(existing: Bool) async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.files["C:\\fail.bin"] = Data(repeating: 0x42, count: 600_000)
        f.fake.failedFileReadOffset = 524_288
        let dest = f.shared + "/existing.bin"
        let original = Data("keep this file".utf8)
        if existing { try write(dest, original) }
        let error = try #require(await f.failure(Method.copyFromVM,
            ["vm": "win11", "guestPath": "C:\\fail.bin", "hostPath": .string(dest)]))
        #expect(error.message.contains("C:\\fail.bin") && error.message.contains("524288"))
        if existing {
            #expect(try Data(contentsOf: URL(fileURLWithPath: dest)) == original)
        } else {
            #expect(!FileManager.default.fileExists(atPath: dest))
        }
        #expect(try FileManager.default.contentsOfDirectory(atPath: f.shared) == (existing ? ["existing.bin"] : []))
    }

    @Test func copyFromVMOfAFileIntoAnExistingDirectoryKeepsTheName() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.files["C:\\out\\r.txt"] = Data("r".utf8)
        try mkdirs(f.shared + "/d")
        _ = try await f.client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\out\\r.txt", hostPath: f.shared + "/d"), as: Results.Copy.self)
        #expect(try String(contentsOfFile: f.shared + "/d/r.txt", encoding: .utf8) == "r")
    }

    @Test func hostPathsOutsideTheAllowlistAreRefusedIncludingDotDotAndSymlinks() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let outside = f.home.path + "/outside"
        try write(outside + "/secret.txt", Data("s".utf8))
        try mkdirs(f.shared)
        try FileManager.default.createSymbolicLink(atPath: f.shared + "/escape", withDestinationPath: outside)
        try FileManager.default.createSymbolicLink(atPath: f.shared + "/secret-link.txt", withDestinationPath: outside + "/secret.txt")
        f.fake.files["C:\\g.txt"] = Data("g".utf8)

        let bad = [
            "/etc/hosts", outside + "/secret.txt",
            f.shared + "/../outside/secret.txt",          // ".." that exists
            f.shared + "/nothing/../../outside/x.txt",    // ".." below a missing directory
            f.shared + "/escape/secret.txt",              // through a directory symlink
            f.shared + "/secret-link.txt",                // a symlink to a file
            "relative/path.txt",
        ]
        for path in bad {
            let to = await f.failure(Method.copyToVM, ["vm": "win11", "hostPath": .string(path), "guestPath": "C:\\x"])
            #expect(to != nil, "copy_to_vm accepted \(path)")
            let from = await f.failure(Method.copyFromVM, ["vm": "win11", "guestPath": "C:\\g.txt", "hostPath": .string(path)])
            #expect(from != nil, "copy_from_vm accepted \(path)")
        }
        let message = try #require(await f.failure(Method.copyToVM, ["vm": "win11", "hostPath": "/etc/hosts", "guestPath": "C:\\x"])).message
        #expect(message.contains("allowlist") && message.contains("/shared"))
        #expect(f.fake.files.keys.sorted() == ["C:\\g.txt"], "nothing may reach the guest")
        #expect(try Data(contentsOf: URL(fileURLWithPath: outside + "/secret.txt")) == Data("s".utf8), "the file outside was overwritten")
        #expect(!FileManager.default.fileExists(atPath: outside + "/x.txt"))
        // Writing through the symlink directory is refused too.
        let write = await f.failure(Method.copyFromVM, ["vm": "win11", "guestPath": "C:\\g.txt", "hostPath": .string(f.shared + "/escape/pwn.txt")])
        #expect(write != nil && !FileManager.default.fileExists(atPath: outside + "/pwn.txt"))
    }

    @Test func theDefaultAllowlistDirectoriesAreCreatedOnDemand() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        #expect(!FileManager.default.fileExists(atPath: f.shared) && !FileManager.default.fileExists(atPath: f.home.path + "/scratch"))
        f.fake.files["C:\\g.txt"] = Data("g".utf8)
        _ = try await f.client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\g.txt", hostPath: f.home.path + "/scratch/g.txt"), as: Results.Copy.self)
        #expect(FileManager.default.fileExists(atPath: f.shared) && FileManager.default.fileExists(atPath: f.home.path + "/scratch/g.txt"))
    }

    @Test func aConfiguredAllowlistReplacesTheDefaults() async throws {
        let extra = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: extra) }
        let f = try await GuestServiceFixture(allowlist: [extra])
        defer { f.cleanup() }
        f.fake.files["C:\\g.txt"] = Data("g".utf8)
        _ = try await f.client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\g.txt", hostPath: extra + "/g.txt"), as: Results.Copy.self)
        let refused = await f.failure(Method.copyFromVM, ["vm": "win11", "guestPath": "C:\\g.txt", "hostPath": .string(f.shared + "/g.txt")])
        #expect(refused != nil)
    }
}
