import CryptoKit
import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon

private func sha(_ s: String) -> String { SHA256.hash(data: Data(s.utf8)).map { String(format: "%02x", $0) }.joined() }

/// The host side of a share sync: the walk and its stat-key hash cache.
@Suite struct HostTreeTests {
    private struct Tree {
        let base: String
        var root: String { base + "/src" }
        var cache: String { base + "/cache/src.json" }
        func put(_ rel: String, _ text: String) throws {
            let p = root + "/" + rel
            try FileManager.default.createDirectory(atPath: (p as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
            try Data(text.utf8).write(to: URL(fileURLWithPath: p))
        }
        func scan(_ exclude: [String] = [], scope: String = "", verify: Bool = false) throws -> HostTree {
            try HostTree.scan(root: root, scope: scope, excludes: ShareExcludes(exclude), cachePath: cache, verify: verify)
        }
    }

    private func tree() throws -> Tree { Tree(base: try shortTempDir()) }

    @Test func aScanListsFilesAndDirectoriesSkippingExcludedAndUnsyncablePaths() throws {
        let t = try tree()
        defer { try? FileManager.default.removeItem(atPath: t.base) }
        try t.put("a.txt", "a")
        try t.put("src/b.cs", "bb")
        try t.put("bin/x.dll", "x")
        try t.put("bad:name.txt", "?")
        try t.put("trailing.", "?")
        try t.put("CON.txt", "?")
        try FileManager.default.createSymbolicLink(atPath: t.root + "/link", withDestinationPath: "/etc")
        let s = try t.scan(["bin/"])
        #expect(s.files == ["a.txt": .init(size: 1, hash: sha("a")), "src/b.cs": .init(size: 2, hash: sha("bb"))])
        #expect(s.dirs == ["src"])
        #expect(s.skipped.sorted() == ["CON.txt", "bad:name.txt", "link", "trailing."])
        #expect(s.hashed == 2)
    }

    @Test func hashesAreRecomputedOnlyWhenTheStatKeyChanges() throws {
        let t = try tree()
        defer { try? FileManager.default.removeItem(atPath: t.base) }
        try t.put("same.txt", "same")
        try t.put("edit.txt", "1234")
        #expect(try t.scan().hashed == 2)
        #expect(try t.scan().hashed == 0, "nothing changed")
        // Same size and mtime (touch -r): the ctime moves, so it is hashed again.
        let path = t.root + "/edit.txt"
        let mtime = try #require(try FileManager.default.attributesOfItem(atPath: path)[.modificationDate] as? Date)
        Thread.sleep(forTimeInterval: 0.02)
        try Data("abcd".utf8).write(to: URL(fileURLWithPath: path))
        try FileManager.default.setAttributes([.modificationDate: mtime], ofItemAtPath: path)
        let s = try t.scan()
        #expect(s.hashed == 1 && s.files["edit.txt"]?.hash == sha("abcd"))
        #expect(try t.scan(verify: true).hashed == 2, "verify rehashes everything")
    }

    @Test func aScopedScanKeepsTheCacheOutsideItAndACacheOfAnotherFolderIsIgnored() throws {
        let t = try tree()
        defer { try? FileManager.default.removeItem(atPath: t.base) }
        try t.put("a/x", "x")
        try t.put("b/y", "y")
        _ = try t.scan()
        let a = try t.scan(scope: "a")
        #expect(a.files.keys.sorted() == ["a/x"] && a.dirs == [] && a.hashed == 0)
        #expect(try t.scan().hashed == 0)
        let moved = t.base + "/moved"
        try FileManager.default.moveItem(atPath: t.root, toPath: moved)
        let other = try HostTree.scan(root: moved, scope: "", excludes: ShareExcludes([]), cachePath: t.cache, verify: false)
        #expect(other.hashed == 2)
    }

    @Test func aSymlinkToADirectoryOutsideTheShareIsNeverTraversed() throws {
        let t = try tree()
        defer { try? FileManager.default.removeItem(atPath: t.base) }
        try t.put("in.txt", "i")
        let outside = t.base + "/outside"
        try FileManager.default.createDirectory(atPath: outside + "/.ssh", withIntermediateDirectories: true)
        try Data("secret".utf8).write(to: URL(fileURLWithPath: outside + "/.ssh/id"))
        try FileManager.default.createSymbolicLink(atPath: t.root + "/escape", withDestinationPath: outside)
        let s = try t.scan()
        #expect(s.files.keys.sorted() == ["in.txt"] && s.dirs.isEmpty && s.skipped == ["escape"])
        #expect(throws: HostTree.OutsideShare.self) { try t.scan(scope: "escape") }
        #expect(throws: HostTree.OutsideShare.self) { try t.scan(scope: "escape/.ssh") }
        #expect(throws: HostTree.OutsideShare.self) { try t.scan(scope: "../outside") }
    }

    @Test func aFileSwappedForASymlinkAfterTheScanIsNotOpened() throws {
        let t = try tree()
        defer { try? FileManager.default.removeItem(atPath: t.base) }
        try t.put("d/f.txt", "f")
        let fd = try HostTree.openBelow(root: t.root, "d/f.txt", directory: false)
        close(fd)
        try Data("secret".utf8).write(to: URL(fileURLWithPath: t.base + "/secret"))
        try FileManager.default.removeItem(atPath: t.root + "/d/f.txt")
        try FileManager.default.createSymbolicLink(atPath: t.root + "/d/f.txt", withDestinationPath: t.base + "/secret")
        #expect(throws: HostTree.OutsideShare.self) { try HostTree.openBelow(root: t.root, "d/f.txt", directory: false) }
        // A directory on the way swapped for a link is refused the same way.
        try FileManager.default.moveItem(atPath: t.root + "/d", toPath: t.base + "/d-moved")
        try FileManager.default.createSymbolicLink(atPath: t.root + "/d", withDestinationPath: t.base + "/d-moved")
        #expect(throws: HostTree.OutsideShare.self) { try HostTree.openBelow(root: t.root, "d/f.txt", directory: false) }
    }

    @Test func aCancelledScanStopsAndLeavesTheCacheAlone() throws {
        let t = try tree()
        defer { try? FileManager.default.removeItem(atPath: t.base) }
        try t.put("a.txt", "a")
        try t.put("b.txt", "b")
        var seen = 0
        #expect(throws: CancellationError.self) {
            try HostTree.scan(root: t.root, scope: "", excludes: ShareExcludes([]), cachePath: t.cache, verify: false) {
                seen += 1
                return seen > 1
            }
        }
        #expect(!FileManager.default.fileExists(atPath: t.cache))
        #expect(try t.scan().hashed == 2)
    }
}
