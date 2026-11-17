import Testing

@testable import GlovCore

@Suite struct SyncPlanTests {
    private typealias H = SyncPlan.HostFile
    private typealias G = SyncPlan.GuestFile

    private func plan(
        host: [String: H], hostDirs: Set<String> = [], guest: [String: G], guestDirs: Set<String> = [], exclude: [String] = [], untouched: [String] = []
    ) throws -> SyncPlan {
        SyncPlan(hostFiles: host, hostDirs: hostDirs, guestFiles: guest, guestDirs: guestDirs, excludes: try ShareExcludes(exclude), untouched: untouched)
    }

    @Test func onlyNewAndChangedFilesAreWrittenAndHostDeletionsAreDeleted() throws {
        let p = try plan(
            host: ["same": H(size: 1, hash: "a"), "changed": H(size: 1, hash: "b"), "new": H(size: 1, hash: "c")],
            guest: ["same": G(size: 1, hash: "a"), "changed": G(size: 1, hash: "x"), "gone": G(size: 1, hash: "d")])
        #expect(p.writes == ["changed", "new"])
        #expect(p.deletes == ["gone"])
        #expect(p.unchanged == 1)
        #expect(p.overwritten.isEmpty)
    }

    @Test func theHashDecidesNotTheSizeAndAnUnknownHashIsAWrite() throws {
        let p = try plan(
            host: ["a": H(size: 5, hash: "h1"), "b": H(size: 5, hash: "h2")],
            guest: ["a": G(size: 5, hash: "h0"), "b": G(size: 5, hash: "")])
        #expect(p.writes == ["a", "b"])
    }

    @Test func excludedGuestPathsAreNeitherWrittenNorDeleted() throws {
        let p = try plan(
            host: ["src/a.cs": H(size: 1, hash: "a"), "bin/x": H(size: 1, hash: "x")], hostDirs: ["src", "bin"],
            guest: ["bin/z": G(size: 1, hash: "z"), "root-only/y": G(size: 1, hash: "y"), "sub/root-only/y": G(size: 1, hash: "q")],
            guestDirs: ["bin", "root-only", "sub", "sub/root-only"], exclude: ["bin/", "/root-only"])
        #expect(p.writes == ["src/a.cs"], "the host's bin/x is excluded")
        #expect(p.deletes == ["sub/root-only/y"], "only the unanchored copy is not excluded")
        #expect(p.rmdirs == ["sub/root-only", "sub"])
        #expect(p.mkdirs == ["src"])
    }

    @Test func untouchedHostPathsKeepTheirGuestCopies() throws {
        let p = try plan(host: [:], guest: ["link/f": G(size: 1, hash: "f"), "link": G(size: 1, hash: "l"), "other": G(size: 1, hash: "o")],
            guestDirs: [], untouched: ["link"])
        #expect(p.deletes == ["other"])
    }

    @Test func directoriesAreCreatedTopDownAndRemovedBottomUp() throws {
        let p = try plan(
            host: ["a/b/c/f": H(size: 1, hash: "f")], hostDirs: ["a", "a/b", "a/b/c"],
            guest: ["x/y/z/g": G(size: 1, hash: "g")], guestDirs: ["a", "x", "x/y", "x/y/z"])
        #expect(p.mkdirs == ["a/b", "a/b/c"])
        #expect(p.rmdirs == ["x/y/z", "x/y", "x"])
    }

    @Test func aPathThatChangedBetweenFileAndDirectoryIsReplaced() throws {
        let p = try plan(
            host: ["was-dir": H(size: 1, hash: "f"), "was-file/inner": H(size: 1, hash: "i")], hostDirs: ["was-file"],
            guest: ["was-file": G(size: 1, hash: "w"), "was-dir/old": G(size: 1, hash: "o")], guestDirs: ["was-dir"])
        #expect(p.deletes == ["was-dir/old", "was-file"])
        #expect(p.rmdirs == ["was-dir"])
        #expect(p.mkdirs == ["was-file"])
        #expect(p.writes == ["was-dir", "was-file/inner"])
    }

    @Test func guestChangesThatAreReplacedOrRemovedAreListed() throws {
        let p = try plan(
            host: ["edited": H(size: 1, hash: "host"), "same": H(size: 1, hash: "s")],
            guest: [
                "edited": G(size: 1, hash: "guest", modified: true), "same": G(size: 1, hash: "s", modified: true),
                "created": G(size: 1, hash: "c", modified: true),
            ])
        #expect(p.overwritten == ["created", "edited"], "an edit that matches the host is no loss")
    }
}
